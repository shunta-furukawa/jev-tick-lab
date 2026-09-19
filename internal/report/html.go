package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"strings"

	"github.com/shunta-furukawa/jev-tick-lab/internal/calib"
)

// HTML renders the report as one self-contained page.
//
// No CDN, no fonts, no scripts from anywhere: the file has to open from a
// laptop, from a GCS bucket, and from an archive in a year's time. Everything
// it needs is inline.
//
// Colours come from the validated palette. The chart marks use one hue; the
// status colours appear only in stat tiles, always beside a word, because
// status-good and status-critical are four Delta E apart under deuteranopia and
// must never carry meaning alone.
func (r Report) HTML() (string, error) {
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, r.view()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type tile struct {
	Label, Value, Note, Status string
}

type section struct {
	Title, Note string
	SVG         template.HTML
	Table       [][]string
	Head        []string
}

type pageView struct {
	Title    string
	Subtitle string
	Meta     [][2]string
	Tiles    []tile
	Sections []section
	Warning  string
}

func (r Report) view() pageView {
	v := pageView{
		Title:    "jev-tick-lab",
		Subtitle: r.Pair,
	}
	if r.Records == 0 {
		v.Warning = "No records. Has the collector written anything yet?"
		return v
	}

	v.Meta = [][2]string{
		{"window", fmt.Sprintf("%s → %s UTC", r.From.Format("2006-01-02 15:04"), r.To.Format("15:04"))},
		{"model", strings.Join(r.Models, ", ")},
		{"cadence", r.Options.TickInterval.String()},
		{"runs", fmt.Sprint(len(r.Runs))},
		{"generated", r.Generated.Format("2006-01-02 15:04 UTC")},
	}

	density := status("good", r.Density >= 0.95)
	fails := status("good", r.FailRate <= 0.02)
	v.Tiles = []tile{
		{"records", human(r.Records), fmt.Sprintf("over %s", round(r.To.Sub(r.From))), ""},
		{"tick density", pct(r.Density), "of the ticks the span should hold", density},
		{"failed calls", fmt.Sprintf("%.2f%%", r.FailRate*100), fmt.Sprintf("%d of %d", r.Failed, r.Records), fails},
		{"latency p50", fmt.Sprintf("%.0f ms", r.Latency50), fmt.Sprintf("p99 %.0f ms", r.Latency99), ""},
		{"cost so far", fmt.Sprintf("$%.2f", r.CostUSD), fmt.Sprintf("$%.2f/day at this cadence", r.CostPerDay), ""},
		{"input tokens", human(r.InputTokens), fmt.Sprintf("%.0f per call", perCall(r)), ""},
	}

	if len(r.Runs) > 1 {
		v.Warning = fmt.Sprintf("This window spans %d runs — the collector restarted. "+
			"Check build_revision in runs-*.jsonl before comparing across the restart.", len(r.Runs))
	}
	if len(r.Models) > 1 {
		v.Warning = fmt.Sprintf("This window spans %d model versions (%s). Records either side are not comparable.",
			len(r.Models), strings.Join(r.Models, ", "))
	}

	v.Sections = append(v.Sections, r.timelineSection(), r.gateSection())
	for _, d := range r.Dists {
		v.Sections = append(v.Sections, d.section())
	}
	if r.HasOutcomes {
		v.Sections = append(v.Sections, r.calibrationSection())
	}
	return v
}

func perCall(r Report) float64 {
	answered := r.Records - r.Failed
	if answered == 0 {
		return 0
	}
	return float64(r.InputTokens) / float64(answered)
}

func status(good string, ok bool) string {
	if ok {
		return good
	}
	return "critical"
}

// --- sections ---------------------------------------------------------------

func (r Report) timelineSection() section {
	s := section{
		Title: "Collection over time",
		Note:  "Share of the ticks each slice should hold. A dip is lost ticks; a gap is the collector not running.",
		Head:  []string{"from", "records", "expected", "density", "failed"},
	}
	for _, b := range r.Timeline {
		s.Table = append(s.Table, []string{
			b.At.Format("01-02 15:04"), fmt.Sprint(b.Records), fmt.Sprint(b.Expected),
			pct(b.Density), fmt.Sprint(b.Errors),
		})
	}
	s.SVG = svgTimeline(r.Timeline)
	return s
}

func (r Report) gateSection() section {
	s := section{
		Title: "What stopped each tick",
		Note: "Shadow mode acts on none of this — the gates are a derived column, so a threshold " +
			"can be re-scored against the same records later.",
		Head: []string{"gate", "ticks", "share"},
	}
	for _, c := range r.Gates {
		s.Table = append(s.Table, []string{c.Label, fmt.Sprint(c.N), pct(c.Share)})
	}
	s.SVG = svgBars(r.Gates)
	return s
}

func (d Dist) section() section {
	note := fmt.Sprintf("%d answers, median %.2f.", d.N, d.Median)
	if d.Gate > 0 {
		side := "above"
		if !d.GateAbove {
			side = "below"
		}
		note += fmt.Sprintf(" %s sits at %.2f; %d answers (%.0f%%) fall %s it.",
			d.GateLabel, d.Gate, d.OverGate, float64(d.OverGate)/float64(d.N)*100, side)
	}
	s := section{
		Title: "Answers: " + d.Question,
		Note:  note,
		Head:  []string{"range", "answers", "share"},
	}
	for _, b := range d.Bins {
		s.Table = append(s.Table, []string{
			fmt.Sprintf("%.2f–%.2f", b.Lo, b.Hi), fmt.Sprint(b.N), pct(b.Share),
		})
	}
	s.SVG = svgHist(d)
	return s
}

func (r Report) calibrationSection() section {
	c := r.Calibration
	note := fmt.Sprintf("%s at +%ds, band %.0f bps. %d usable of %d records. "+
		"Base rate %.3f, Brier %.4f, expected calibration error %.4f. "+
		"A point above the diagonal happened more often than the model said; below, less.",
		c.Options.QuestionID, c.Options.HorizonSec, c.Options.BandBps,
		c.Usable, c.Total, c.BaseRate, c.Brier, c.ECE)

	s := section{Title: "Reliability — stated against realised", Note: note,
		Head: []string{"bucket", "n", "stated", "realised", "gap"}}
	for _, b := range c.Bins {
		if b.N == 0 {
			s.Table = append(s.Table, []string{fmt.Sprintf("%.1f–%.1f", b.Lo, b.Hi), "0", "—", "—", "—"})
			continue
		}
		s.Table = append(s.Table, []string{
			fmt.Sprintf("%.1f–%.1f", b.Lo, b.Hi), fmt.Sprint(b.N),
			fmt.Sprintf("%.3f", b.MeanPredicted), fmt.Sprintf("%.3f", b.Realised),
			fmt.Sprintf("%+.3f", b.Gap()),
		})
	}
	s.SVG = svgReliability(c)
	return s
}

// --- svg --------------------------------------------------------------------

const (
	chartW = 720.0
	chartH = 240.0
	padL   = 44.0
	padR   = 16.0
	padT   = 14.0
	padB   = 30.0
)

func svgOpen(w, h float64) *strings.Builder {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" preserveAspectRatio="xMidYMid meet" role="img" class="chart">`, w, h)
	return &b
}

// gridlines and the baseline, both recessive.
func axes(b *strings.Builder, w, h float64, labels []string) {
	for i := 0; i <= 4; i++ {
		y := padT + (h-padT-padB)*float64(i)/4
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, y, w-padR, y)
		if i < len(labels) {
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
				padL-8, y+4, template.HTMLEscapeString(labels[i]))
		}
	}
	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="baseline"/>`,
		padL, h-padB, w-padR, h-padB)
}

func svgTimeline(buckets []Bucket) template.HTML {
	if len(buckets) == 0 {
		return ""
	}
	b := svgOpen(chartW, chartH)
	axes(b, chartW, chartH, []string{"100%", "75%", "50%", "25%", "0"})

	plotW := chartW - padL - padR
	plotH := chartH - padT - padB
	step := plotW / float64(len(buckets))
	barW := math.Max(2, step-2) // a 2px surface gap between fills

	for i, bk := range buckets {
		x := padL + float64(i)*step
		bh := plotH * bk.Density
		if bh < 1 && bk.Records > 0 {
			bh = 1
		}
		y := padT + plotH - bh
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="mark" `+
			`tabindex="0" data-tip="%s · %d records of %d · %s"><title>%s — %d of %d (%s)</title></rect>`,
			x, y, barW, math.Max(bh, 0),
			template.HTMLEscapeString(bk.At.Format("01-02 15:04")), bk.Records, bk.Expected, pct(bk.Density),
			template.HTMLEscapeString(bk.At.Format("01-02 15:04")), bk.Records, bk.Expected, pct(bk.Density))
	}
	// First and last time, directly labelled rather than a full axis.
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis">%s</text>`,
		padL, chartH-10, template.HTMLEscapeString(buckets[0].At.Format("01-02 15:04")))
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
		chartW-padR, chartH-10, template.HTMLEscapeString(buckets[len(buckets)-1].At.Format("15:04")))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func svgBars(rows []Count) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	rowH := 30.0
	h := padT + float64(len(rows))*rowH + 10
	labelW := 150.0
	b := svgOpen(chartW, h)

	max := 0
	for _, r := range rows {
		if r.N > max {
			max = r.N
		}
	}
	plotW := chartW - labelW - padR - 60

	for i, r := range rows {
		y := padT + float64(i)*rowH
		w := 0.0
		if max > 0 {
			w = plotW * float64(r.N) / float64(max)
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="label" text-anchor="end">%s</text>`,
			labelW-10, y+18, template.HTMLEscapeString(r.Label))
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="14" rx="4" class="mark" `+
			`tabindex="0" data-tip="%s · %d ticks · %s"><title>%s — %d (%s)</title></rect>`,
			labelW, y+6, math.Max(w, 2),
			template.HTMLEscapeString(r.Label), r.N, pct(r.Share),
			template.HTMLEscapeString(r.Label), r.N, pct(r.Share))
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="value">%d · %s</text>`,
			labelW+math.Max(w, 2)+8, y+18, r.N, pct(r.Share))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func svgHist(d Dist) template.HTML {
	if len(d.Bins) == 0 {
		return ""
	}
	h := 206.0
	b := svgOpen(chartW, h)

	maxShare := 0.0
	for _, bin := range d.Bins {
		if bin.Share > maxShare {
			maxShare = bin.Share
		}
	}
	axes(b, chartW, h, []string{pct(maxShare), "", "", "", "0"})

	plotW := chartW - padL - padR
	plotH := h - padT - padB
	step := plotW / float64(len(d.Bins))
	barW := math.Max(2, step-2)

	for i, bin := range d.Bins {
		x := padL + float64(i)*step
		bh := 0.0
		if maxShare > 0 {
			bh = plotH * bin.Share / maxShare
		}
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="mark" `+
			`tabindex="0" data-tip="%.2f–%.2f · %d answers · %s"><title>%.2f–%.2f — %d (%s)</title></rect>`,
			x, padT+plotH-bh, barW, bh,
			bin.Lo, bin.Hi, bin.N, pct(bin.Share),
			bin.Lo, bin.Hi, bin.N, pct(bin.Share))
	}

	// The gate is a reference, not a series: a dashed neutral rule, never a hue
	// that could be mistaken for data. It is drawn over the bars, so it carries
	// a surface-coloured casing to stay legible against them, and its label
	// sits in the top padding where no mark can reach it.
	if d.Gate > 0 && d.Max > 0 {
		x := padL + plotW*d.Gate/d.Max
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="refcasing"/>`, x, padT, x, padT+plotH)
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`, x, padT, x, padT+plotH)

		anchor, dx := "start", 6.0
		if x > chartW*0.65 {
			anchor, dx = "end", -6.0
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="%s">%s %.2f</text>`,
			x+dx, padT-3, anchor, template.HTMLEscapeString(d.GateLabel), d.Gate)
	}

	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis">0</text>`, padL, h-10)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%.0f</text>`, chartW-padR, h-10, d.Max)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// svgReliability is the headline chart: what the model said against what
// happened. The diagonal is perfect calibration — a reference, drawn in neutral
// ink, never a series.
//
// It uses the same viewBox width as every other chart on the page. That is not
// cosmetic: the charts are sized by CSS at 100% width, so a narrower viewBox
// scales up more, and a square 360-wide canvas rendered type at twice the size
// of its neighbours and overflowed its own frame.
func svgReliability(c calib.Report) template.HTML {
	h := 340.0
	plot := h - padT - padB - 16 // square, and it sets the height budget
	left := (chartW - plot) / 2  // centred, so the page rhythm holds
	top := padT + 8

	b := svgOpen(chartW, h)
	for i := 0; i <= 4; i++ {
		f := float64(i) / 4
		y := top + plot*f
		x := left + plot*f
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, left, y, left+plot, y)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%.2f</text>`, left-8, y+4, 1-f)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%.1f</text>`, x, top+plot+16, f)
	}

	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`,
		left, top+plot, left+plot, top)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="start">perfect calibration</text>`,
		left+plot+8, top+4)

	maxN := 0
	for _, bin := range c.Bins {
		if bin.N > maxN {
			maxN = bin.N
		}
	}

	var path []string
	for _, bin := range c.Bins {
		if bin.N == 0 {
			continue
		}
		path = append(path, fmt.Sprintf("%.1f,%.1f",
			left+plot*bin.MeanPredicted, top+plot*(1-bin.Realised)))
	}
	if len(path) > 1 {
		fmt.Fprintf(b, `<polyline points="%s" class="line"/>`, strings.Join(path, " "))
	}
	for _, bin := range c.Bins {
		if bin.N == 0 {
			continue
		}
		rad := 5.0
		if maxN > 0 {
			rad = 4 + 6*math.Sqrt(float64(bin.N)/float64(maxN))
		}
		fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f" class="dot" tabindex="0" `+
			`data-tip="stated %.3f · realised %.3f · n=%d · gap %+.3f">`+
			`<title>stated %.3f, realised %.3f, n=%d (gap %+.3f)</title></circle>`,
			left+plot*bin.MeanPredicted, top+plot*(1-bin.Realised), rad,
			bin.MeanPredicted, bin.Realised, bin.N, bin.Gap(),
			bin.MeanPredicted, bin.Realised, bin.N, bin.Gap())
	}

	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">stated probability</text>`,
		left+plot/2, h-6)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle" transform="rotate(-90 %.1f %.1f)">realised</text>`,
		left-46, top+plot/2, left-46, top+plot/2)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
