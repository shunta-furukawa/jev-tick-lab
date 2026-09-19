package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

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

	// Summary names the disclosure; Open shows it expanded. Both exist for the
	// tape, which is the one table on the page worth reading directly rather
	// than keeping behind a click.
	Summary string
	Open    bool
}

type pageView struct {
	Title    string
	Subtitle string
	Meta     [][2]string
	Tiles    []tile
	Sections []section
	Warnings []string
	Live     bool
	LiveNote string
}

func (r Report) view() pageView {
	v := pageView{
		Title:    "jev-tick-lab",
		Subtitle: r.Pair,
		Live:     r.Live,
	}
	if r.Live {
		age := time.Since(r.To)
		v.LiveNote = fmt.Sprintf("newest record %s ago", round(age))
		if age > 2*time.Minute {
			v.LiveNote += " — the collector may have stopped"
		}
	}
	if r.Records == 0 {
		v.Warnings = []string{"No records. Has the collector written anything yet?"}
		return v
	}

	v.Meta = [][2]string{
		{"window", fmt.Sprintf("%s → %s UTC", r.From.Format("2006-01-02 15:04"), r.To.Format("15:04"))},
		{"model", strings.Join(r.Models, ", ")},
		{"cadence", r.Options.TickInterval.String()},
		{"runs", fmt.Sprint(len(r.Runs))},
		{"generated", r.Generated.Format("2006-01-02 15:04 UTC")},
	}

	// Not just "at least 95%": over 100% means the cadence this page was given
	// is not the one in the data, and a green dot beside 297% is a page
	// arguing with itself.
	density := status("good", r.Density >= 0.95 && r.Density <= 1.05)
	fails := status("good", r.FailRate <= 0.02)
	v.Tiles = []tile{
		{"records", human(r.Records), fmt.Sprintf("over %s", round(r.To.Sub(r.From))), ""},
		{"tick density", pct(r.Density), "of the ticks the span should hold", density},
		{"failed calls", fmt.Sprintf("%.2f%%", r.FailRate*100), fmt.Sprintf("%d of %d", r.Failed, r.Records), fails},
		{"latency p50", fmt.Sprintf("%.0f ms", r.Latency50), fmt.Sprintf("p99 %.0f ms", r.Latency99), ""},
		{"cost so far", fmt.Sprintf("$%.2f", r.CostUSD), fmt.Sprintf("$%.2f/day at this cadence", r.CostPerDay), ""},
		{"input tokens", human(r.InputTokens), fmt.Sprintf("%.0f per call", perCall(r)), ""},
	}

	// The cadence is an input, not a measurement, and every "expected" count on
	// this page hangs off it. Say so when the data disagrees, rather than
	// reporting a healthy run as a broken one.
	if o, want := r.ObservedTick, r.Options.TickInterval; o > 0 && want > 0 {
		if ratio := o.Seconds() / want.Seconds(); ratio < 0.75 || ratio > 1.33 {
			v.Warnings = append(v.Warnings, fmt.Sprintf(
				"This page was told the cadence is %s, but the records are %s apart. "+
					"Every expected-count and density figure below is wrong until -tick says %s.",
				want, o, o))
		}
	}
	// In paper mode the P&L is the thing you opened the page for, so it goes
	// in front of the collection statistics rather than below them.
	if len(r.Paper) > 0 {
		v.Tiles = append(paperTiles(r), v.Tiles...)
	}

	if len(r.Runs) > 1 {
		v.Warnings = append(v.Warnings, fmt.Sprintf("This window spans %d runs — the collector restarted. "+
			"Check build_revision in runs-*.jsonl before comparing across the restart.", len(r.Runs)))
	}
	if len(r.Models) > 1 {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"This window spans %d model versions (%s). Records either side are not comparable.",
			len(r.Models), strings.Join(r.Models, ", ")))
	}

	if len(r.Recent) > 0 {
		v.Sections = append(v.Sections, r.priceSection())
	}
	if len(r.Paper) > 0 {
		v.Sections = append(v.Sections, r.paperSection())
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

// paperTiles are the numbers a paper run is actually about.
func paperTiles(r Report) []tile {
	var out []tile
	for _, p := range r.Paper {
		net := p.NetJPY + p.UnrealJPY
		note := fmt.Sprintf("%d round trip(s), %d up", p.Trips, p.Wins)
		if p.Working > 0 {
			note += fmt.Sprintf(", %d working", p.Working)
		}
		out = append(out, tile{
			// The unit goes in the label. A stat tile is 24px type in a 140px
			// column, and "+1,234.56 JPY" wraps onto a second line there,
			// which strands the status dot on a line of its own.
			Label:  p.Style + " net (JPY)",
			Value:  fmt.Sprintf("%+.2f", net),
			Note:   note,
			Status: status("good", net > 0),
		})
	}
	// The gap between the two is the entire point of running both, so it is
	// its own tile rather than something to be worked out by subtraction.
	if len(r.Paper) == 2 {
		a, b := r.Paper[0], r.Paper[1]
		out = append(out, tile{
			Label: "fees paid (JPY)",
			Value: fmt.Sprintf("%+.2f", a.FeesJPY+b.FeesJPY),
			Note:  fmt.Sprintf("%s %s, %s %s", a.Style, jpy(a.FeesJPY), b.Style, jpy(b.FeesJPY)),
		})
	}
	return out
}

// paperSection lays the paths side by side. Gross before fees against net
// after them is the comparison the whole phase exists to make: an all-taker
// round trip on a JPY alt is 24bps, which is larger than most of the moves
// this experiment is trying to predict.
func (r Report) paperSection() section {
	note := "The same answers, executed two ways. Taker crosses the spread and always fills; " +
		"maker rests at the touch, earns the rebate, and often does not fill at all. " +
		"Gross is before fees, net is after — the difference is the cost of getting in and out."
	for _, p := range r.Paper {
		if p.Working > 0 || p.Size > 0 {
			continue
		}
		if p.Trips == 0 {
			note += fmt.Sprintf(" The %s path has not completed a round trip yet.", p.Style)
		}
	}

	s := section{
		Title:   "Paper execution — maker against taker",
		Note:    note,
		Summary: "Per path",
		Open:    true,
		Head:    []string{"path", "position", "round trips", "up", "gross", "fees", "net", "unrealised"},
	}
	for _, p := range r.Paper {
		pos := "flat"
		if p.Size > 0 {
			pos = fmt.Sprintf("%s %.4f @ %.3f", p.Side, p.Size, p.EntryPx)
		}
		if p.Working > 0 {
			pos += fmt.Sprintf(" (+%d working)", p.Working)
		}
		s.Table = append(s.Table, []string{
			p.Style, pos, fmt.Sprint(p.Trips), fmt.Sprint(p.Wins),
			jpy(p.GrossJPY), jpy(p.FeesJPY), jpy(p.NetJPY), jpy(p.UnrealJPY),
		})
	}
	s.SVG = svgPaths(r.Paper)
	return s
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

// priceSection is the one part of the page that shows the run happening rather
// than summarising it: the price, tick by tick, with what the model called on
// each one.
//
// It does not show trades, because in shadow mode there are none. Saying
// "0 trades" would be true and useless; what the run actually produces is a
// call per tick and a gate verdict per call, so that is what is drawn.
func (r Report) priceSection() section {
	wanted, failed := 0, 0
	for _, t := range r.Recent {
		if t.Wanted {
			wanted++
		}
		if t.Error != "" {
			failed++
		}
	}

	// The chart draws the data, so it quotes the cadence measured from the
	// data — not the one the caller passed in, which may be wrong.
	cadence := r.ObservedTick
	if cadence <= 0 {
		cadence = r.Options.TickInterval
	}
	note := fmt.Sprintf("The last %d ticks, one dot each, placed at the time it happened — "+
		"so the spacing is the %s cadence and a hole is a tick that never ran.",
		len(r.Recent), cadence)
	if first, last := firstLastPrice(r.Recent); first > 0 && last > 0 {
		note += fmt.Sprintf(" Price %s → %s (%+.1f bps over the window).",
			priceStr(first, r.PriceMax-r.PriceMin), priceStr(last, r.PriceMax-r.PriceMin),
			(last-first)/first*10000)
	}
	paper := len(r.Paper) > 0
	switch {
	case wanted == 0:
		note += " The model asked to wait on every one of them, so nothing here would have been a trade."
	case paper:
		note += fmt.Sprintf(" On %d of them the model asked to enter or exit — ringed, with a rule "+
			"through the plot. What each one actually did is in the paper section below.", wanted)
	default:
		note += fmt.Sprintf(" On %d of them the model asked to enter or exit — ringed, with a rule "+
			"through the plot. Shadow mode fills none of it; the gate column says what would have "+
			"stopped each one.", wanted)
	}
	filled, held := 0, 0
	for _, t := range r.Recent {
		if t.Filled {
			filled++
		}
		if t.Held {
			held++
		}
	}
	if filled > 0 || held > 0 {
		note += fmt.Sprintf(" %d execution(s) here, marked with a diamond; the band along the "+
			"foot is the %d tick(s) a position was open.", filled, held)
	}
	if failed > 0 {
		note += fmt.Sprintf(" %d call(s) in this window failed and have no answer.", failed)
	}

	s := section{
		Title:   "The run, tick by tick",
		Note:    note,
		SVG:     svgPrice(r.Recent, r.PriceMin, r.PriceMax),
		Summary: "Tape — newest first",
		Open:    true,
		Head:    []string{"time (UTC)", "price", "Δ bps", "called", "p", "conf", "gate"},
	}

	span := r.PriceMax - r.PriceMin
	tape, base := r.Recent, 0
	if len(tape) > tapeRows {
		base = len(tape) - tapeRows
		tape = tape[base:]
	}
	for i := len(tape) - 1; i >= 0; i-- { // newest first, the way a tape reads
		t := tape[i]
		called, prob, conf := t.Action, "—", "—"
		if t.Error != "" {
			called = "call failed"
		} else if called == "" {
			called = "—"
		}
		if t.Prob > 0 {
			prob = fmt.Sprintf("%.2f", t.Prob)
		}
		if t.Conf > 0 {
			conf = fmt.Sprintf("%.2f", t.Conf)
		}
		// The first tick in the window has nothing to be a delta against, which
		// is not the same as not having moved.
		delta := "—"
		if t.Price > 0 && base+i > 0 {
			delta = bpsStr(t.DeltaBps)
		}
		s.Table = append(s.Table, []string{
			t.At.Format("15:04:05"), priceStr(t.Price, span), delta, called, prob, conf, t.Gate,
		})
	}
	return s
}

func firstLastPrice(ticks []Tick) (first, last float64) {
	for _, t := range ticks {
		if t.Price <= 0 {
			continue
		}
		if first == 0 {
			first = t.Price
		}
		last = t.Price
	}
	return first, last
}

// bpsStr signs a move, but never signs a move too small to have a direction at
// the precision shown: "%+.1f" renders a two-hundredth of a basis point as
// "-0.0", which reads as a fall that did not happen.
func bpsStr(v float64) string {
	if math.Abs(v) < 0.05 {
		return "0.0"
	}
	return fmt.Sprintf("%+.1f", v)
}

// priceStr picks its decimals from the range on screen rather than the pair, so
// xrp_jpy at 300.123 and btc_jpy at 15,400,000 both read correctly without the
// renderer knowing which is which.
func priceStr(v, span float64) string {
	if v <= 0 {
		return "—"
	}
	d := 0
	switch {
	case span < 0.01:
		d = 4
	case span < 1:
		d = 3
	case span < 10:
		d = 2
	case span < 1000:
		d = 1
	}
	return fmt.Sprintf("%.*f", d, v)
}

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

// tapeRows is how much of the tail the tape prints. The chart covers the whole
// recent window; the table is for reading, and a hundred open rows is not.
const tapeRows = 30

// svgPrice draws the tail of the log as a price line with one dot per
// evaluation, each placed at the time it actually happened.
//
// Placing by time rather than by index is the whole point of the chart: at an
// even cadence the dots are evenly spaced, so the cadence is legible by
// construction and a skipped tick shows up as a gap in the rhythm instead of
// being quietly closed up.
//
// Ticks where the model asked for an entry or an exit are marked with a rule
// and a ring. Both are shape, never hue: this page's two status colours are
// four Delta E apart under deuteranopia and may not carry meaning alone.
func svgPrice(ticks []Tick, lo, hi float64) template.HTML {
	if len(ticks) == 0 || hi <= 0 {
		return ""
	}
	h := 220.0
	b := svgOpen(chartW, h)

	plotW := chartW - padL - padR
	plotH := h - padT - padB

	// A flat window would otherwise divide by zero and draw a line on the axis.
	// Give it a visible band instead, so "nothing moved" looks like nothing
	// moved rather than like missing data.
	span := hi - lo
	if span <= 0 {
		span = math.Max(hi*0.0001, 0.0001)
		lo, hi = hi-span/2, hi+span/2
	}
	pad := span * 0.15
	lo, hi = lo-pad, hi+pad
	span = hi - lo

	y := func(p float64) float64 { return padT + plotH*(hi-p)/span }

	t0, tn := ticks[0].At, ticks[len(ticks)-1].At
	width := tn.Sub(t0).Seconds()
	x := func(i int) float64 {
		if width <= 0 {
			if len(ticks) == 1 {
				return padL + plotW/2
			}
			return padL + plotW*float64(i)/float64(len(ticks)-1)
		}
		return padL + plotW*ticks[i].At.Sub(t0).Seconds()/width
	}

	// Gridlines with the price they stand for, top to bottom.
	for i := 0; i <= 4; i++ {
		gy := padT + plotH*float64(i)/4
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, gy, chartW-padR, gy)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
			padL-8, gy+4, priceStr(hi-span*float64(i)/4, span))
	}

	// A band along the foot of the plot for the stretches a position was
	// actually held. It sits under everything: it is context for the line, not
	// a series of its own.
	for i, t := range ticks {
		if !t.Held {
			continue
		}
		w := plotW / math.Max(float64(len(ticks)-1), 1)
		if width > 0 && i+1 < len(ticks) {
			w = x(i+1) - x(i)
		}
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="6" class="heldband"/>`,
			x(i), padT+plotH-6, math.Max(w, 1))
	}

	// The call rules go down first so the price line reads over them.
	for i, t := range ticks {
		if !t.Wanted {
			continue
		}
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="callrule"/>`,
			x(i), padT, x(i), padT+plotH)
	}

	var pts []string
	for i, t := range ticks {
		if t.Price <= 0 {
			continue
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(i), y(t.Price)))
	}
	if len(pts) > 1 {
		fmt.Fprintf(b, `<polyline points="%s" class="line"/>`, strings.Join(pts, " "))
	}

	for i, t := range ticks {
		if t.Price <= 0 {
			continue
		}
		tip := fmt.Sprintf("%s · %s · %s bps", t.At.Format("15:04:05"), priceStr(t.Price, span), bpsStr(t.DeltaBps))
		if t.Error != "" {
			tip += " · call failed"
		} else if t.Action != "" {
			tip += fmt.Sprintf(" · %s p=%.2f · %s", t.Action, t.Prob, t.Gate)
		}
		fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="2.4" class="tickdot" tabindex="0" data-tip="%s">`+
			`<title>%s</title></circle>`,
			x(i), y(t.Price), template.HTMLEscapeString(tip), template.HTMLEscapeString(tip))
		if t.Wanted {
			fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="5.5" class="callring"/>`, x(i), y(t.Price))
		}
		// A fill is a different event from a call, so it gets a different
		// shape: a filled diamond, legible without colour beside a ring.
		if t.Filled {
			fmt.Fprintf(b, `<path d="M %.1f %.1f l 5 5 l -5 5 l -5 -5 Z" class="fillmark"><title>%s</title></path>`,
				x(i), y(t.Price)-5, template.HTMLEscapeString("executed at "+t.At.Format("15:04:05")))
		}
	}

	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="baseline"/>`,
		padL, padT+plotH, chartW-padR, padT+plotH)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis">%s</text>`,
		padL, h-10, template.HTMLEscapeString(t0.Format("15:04:05")))
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%d ticks</text>`,
		padL+plotW/2, h-10, len(ticks))
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
		chartW-padR, h-10, template.HTMLEscapeString(tn.Format("15:04:05")))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// svgPaths draws gross and net per path as a paired bar, zero-centred.
//
// The pair is the message: a gross bar well above zero beside a net bar below
// it is a strategy that was right and still lost, which is the specific
// failure CLAUDE.md predicts for anything taking liquidity at 24bps a round
// trip. Both bars use the one series hue; they are told apart by position and
// by their labels, never by colour.
func svgPaths(paths []Path) template.HTML {
	if len(paths) == 0 {
		return ""
	}
	rowH, barH := 54.0, 16.0
	h := padT + float64(len(paths))*rowH + 26
	labelW := 92.0
	b := svgOpen(chartW, h)

	span := 0.0
	for _, p := range paths {
		span = math.Max(span, math.Max(math.Abs(p.GrossJPY), math.Abs(p.NetJPY)))
	}
	if span <= 0 {
		span = 1 // nothing has happened yet; draw the axis, not a divide by zero
	}
	// A wide right gutter: the value labels sit outside the plot and
	// "gross -10.22 JPY" is ~95px of type at this size.
	plotW := chartW - labelW - padR - 130
	zero := labelW + plotW/2

	for i, p := range paths {
		y := padT + float64(i)*rowH
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="label" text-anchor="end">%s</text>`,
			labelW-10, y+22, template.HTMLEscapeString(p.Style))

		for j, v := range []struct {
			name string
			val  float64
		}{{"gross", p.GrossJPY}, {"net", p.NetJPY}} {
			by := y + float64(j)*(barH+4)
			w := plotW / 2 * math.Abs(v.val) / span
			x := zero
			if v.val < 0 {
				x = zero - w
			}
			tip := fmt.Sprintf("%s %s: %s", p.Style, v.name, jpy(v.val))
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="mark" `+
				`tabindex="0" data-tip="%s"><title>%s</title></rect>`,
				x, by, math.Max(w, 1.5), barH, template.HTMLEscapeString(tip), template.HTMLEscapeString(tip))
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="value">%s %s</text>`,
				zero+plotW/2+8, by+12, template.HTMLEscapeString(v.name), jpy(v.val))
		}
	}

	// The zero line is a reference, not a series.
	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ref"/>`,
		zero, padT-4, zero, h-22)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="reflabel" text-anchor="middle">break even</text>`,
		zero, h-8)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
