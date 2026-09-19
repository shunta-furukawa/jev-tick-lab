// Package report turns a tick log into something you can look at.
//
// The numbers here are the same ones cmd/logcheck and cmd/calib print as text.
// What this adds is shape: whether the collection thinned out overnight, where
// the anomaly readings actually sit relative to the gate that reads them, and —
// once the log has been through cmd/fill — the reliability curve that is the
// deliverable of the whole experiment.
//
// Everything in this file is pure: records in, a Report out. The HTML lives in
// html.go and the file handling in cmd/report.
package report

import (
	"math"
	"sort"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/calib"
	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

// Options configure one report.
type Options struct {
	TickInterval time.Duration
	HorizonSec   int
	BandBps      float64
	Buckets      int     // timeline resolution
	PricePerMTok float64 // for the cost figure
}

func DefaultOptions() Options {
	return Options{
		TickInterval: 3 * time.Second,
		HorizonSec:   60,
		BandBps:      24, // an all-taker round trip on a JPY alt
		Buckets:      48,
		PricePerMTok: 0.042,
	}
}

// Bucket is one slice of the timeline.
type Bucket struct {
	At       time.Time
	Records  int
	Errors   int
	Expected int
	Density  float64 // records / expected, capped at 1 for display
}

// Count is one row of a categorical breakdown.
type Count struct {
	Label string
	N     int
	Share float64
}

// Bin is one column of a distribution.
type Bin struct {
	Lo, Hi float64
	N      int
	Share  float64
}

// Dist is one question's answer distribution, with the gate that reads it.
type Dist struct {
	Question  string
	Kind      string // noul | score
	Max       float64
	Bins      []Bin
	N         int
	Median    float64
	Gate      float64 // 0 when no gate reads this answer
	GateLabel string
	OverGate  int
	GateAbove bool // true when the gate fires ABOVE the threshold
}

// Report is everything the page draws.
type Report struct {
	Options Options

	Pair      string
	Models    []string
	Runs      []string
	From, To  time.Time
	Generated time.Time

	Records   int
	Failed    int
	FailRate  float64
	Density   float64
	Latency50 float64
	Latency99 float64

	InputTokens int
	CostUSD     float64
	CostPerDay  float64

	Timeline []Bucket
	Gates    []Count
	Intents  []Count
	Dists    []Dist

	// Live is set when the page is being served rather than written to a file:
	// it adds the reload and the freshness line.
	Live bool

	// Filled only when the log has been through cmd/fill.
	HasOutcomes bool
	Calibration calib.Report
}

// noulGates maps a question to the threshold that reads its answer, so the
// distribution can be drawn against the line that actually matters rather than
// against an arbitrary axis.
func noulGates(t decide.Thresholds) map[string]struct {
	gate  float64
	label string
	above bool
} {
	return map[string]struct {
		gate  float64
		label string
		above bool
	}{
		jev.QAnomaly:    {t.MaxAnomalyNoul, "MaxAnomalyNoul", true},
		jev.QFakeout:    {t.MaxFakeoutNoul, "MaxFakeoutNoul", true},
		jev.QHoldRisk:   {t.MaxHoldRisk, "MaxHoldRisk", true},
		jev.QEntryScore: {t.MinEntryQuality, "MinEntryQuality", false},
	}
}

// Build computes the report. Records may be raw or forward-filled; the
// calibration section appears only in the latter case.
func Build(records []obs.Record, opt Options) Report {
	if opt.TickInterval <= 0 {
		opt.TickInterval = 3 * time.Second
	}
	if opt.Buckets <= 0 {
		opt.Buckets = 48
	}

	rep := Report{Options: opt, Generated: time.Now().UTC()}
	if len(records) == 0 {
		return rep
	}

	sorted := append([]obs.Record(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	rep.From, rep.To = sorted[0].At.UTC(), sorted[len(sorted)-1].At.UTC()
	rep.Records = len(sorted)
	rep.Pair = sorted[0].Pair

	var (
		models   = map[string]bool{}
		runs     = map[string]bool{}
		gates    = map[string]int{}
		intents  = map[string]int{}
		latency  []float64
		answers  = map[string][]float64{}
		kinds    = map[string]string{}
		horizons int
	)

	for _, rec := range sorted {
		if rec.RunID != "" {
			runs[rec.RunID] = true
		}
		if rec.Error != "" {
			rep.Failed++
			continue
		}
		if rec.ModelVersion != "" {
			models[rec.ModelVersion] = true
		}
		latency = append(latency, rec.LatencyMs)
		rep.InputTokens += rec.InputTokens
		gates[gateLabel(string(rec.Signal.Gate))]++
		intents[string(rec.Signal.Intent)]++

		if horizonPrice(rec, opt.HorizonSec) > 0 {
			horizons++
		}
		for id, a := range rec.Answers {
			switch a.Type {
			case "noul":
				answers[id] = append(answers[id], a.Noul)
				kinds[id] = "noul"
			case "score":
				answers[id] = append(answers[id], a.Score)
				kinds[id] = "score"
			}
		}
	}

	rep.Models, rep.Runs = keys(models), keys(runs)
	if rep.Records > 0 {
		rep.FailRate = float64(rep.Failed) / float64(rep.Records)
	}
	if len(latency) > 0 {
		rep.Latency50 = calib.Percentile(append([]float64(nil), latency...), 50)
		rep.Latency99 = calib.Percentile(append([]float64(nil), latency...), 99)
	}

	span := rep.To.Sub(rep.From).Seconds() + opt.TickInterval.Seconds()
	if expected := span / opt.TickInterval.Seconds(); expected > 0 {
		rep.Density = float64(rep.Records) / expected
	}
	rep.CostUSD = float64(rep.InputTokens) * opt.PricePerMTok / 1e6
	if span > 0 {
		rep.CostPerDay = rep.CostUSD / span * 86400
	}

	rep.Timeline = timeline(sorted, rep.From, rep.To, opt)
	rep.Gates = counts(gates, rep.Records-rep.Failed)
	rep.Intents = counts(intents, rep.Records-rep.Failed)
	rep.Dists = distributions(answers, kinds, decide.DefaultThresholds())

	// Only claim a calibration section when there is something to calibrate.
	if horizons > 0 {
		rep.HasOutcomes = true
		rep.Calibration, _ = calib.Build(records, calib.Options{
			QuestionID: jev.QAction,
			Outcome:    calib.OutcomeAction,
			HorizonSec: opt.HorizonSec,
			BandBps:    opt.BandBps,
			Bins:       10,
		})
	}
	return rep
}

func timeline(recs []obs.Record, from, to time.Time, opt Options) []Bucket {
	span := to.Sub(from)
	if span <= 0 {
		span = opt.TickInterval
	}
	width := span / time.Duration(opt.Buckets)
	if width < opt.TickInterval {
		width = opt.TickInterval
	}

	out := make([]Bucket, 0, opt.Buckets)
	for start := from; start.Before(to) || start.Equal(to); start = start.Add(width) {
		out = append(out, Bucket{
			At:       start,
			Expected: int(width.Seconds() / opt.TickInterval.Seconds()),
		})
		if len(out) >= opt.Buckets {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}

	for _, rec := range recs {
		i := int(rec.At.Sub(from) / width)
		if i < 0 {
			i = 0
		}
		if i >= len(out) {
			i = len(out) - 1
		}
		out[i].Records++
		if rec.Error != "" {
			out[i].Errors++
		}
	}
	for i := range out {
		if out[i].Expected > 0 {
			out[i].Density = math.Min(1, float64(out[i].Records)/float64(out[i].Expected))
		}
	}
	return out
}

func distributions(answers map[string][]float64, kinds map[string]string, t decide.Thresholds) []Dist {
	gates := noulGates(t)
	ids := make([]string, 0, len(answers))
	for id := range answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var out []Dist
	for _, id := range ids {
		vs := answers[id]
		if len(vs) == 0 {
			continue
		}
		kind := kinds[id]
		max := 1.0
		if kind == "score" {
			max = 4.0
		}

		d := Dist{Question: id, Kind: kind, Max: max, N: len(vs)}
		if g, ok := gates[id]; ok {
			d.Gate, d.GateLabel, d.GateAbove = g.gate, g.label, g.above
		}

		const nbins = 20
		counts := make([]int, nbins)
		for _, v := range vs {
			i := int(v / max * nbins)
			if i >= nbins {
				i = nbins - 1
			}
			if i < 0 {
				i = 0
			}
			counts[i]++
			if d.Gate > 0 {
				if (d.GateAbove && v > d.Gate) || (!d.GateAbove && v < d.Gate) {
					d.OverGate++
				}
			}
		}
		for i, n := range counts {
			d.Bins = append(d.Bins, Bin{
				Lo:    float64(i) / nbins * max,
				Hi:    float64(i+1) / nbins * max,
				N:     n,
				Share: float64(n) / float64(len(vs)),
			})
		}
		s := append([]float64(nil), vs...)
		sort.Float64s(s)
		d.Median = s[len(s)/2]
		out = append(out, d)
	}
	return out
}

func counts(m map[string]int, total int) []Count {
	out := make([]Count, 0, len(m))
	for k, n := range m {
		share := 0.0
		if total > 0 {
			share = float64(n) / float64(total)
		}
		out = append(out, Count{Label: k, N: n, Share: share})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func gateLabel(g string) string {
	if g == "" {
		return "passed every gate"
	}
	return g
}

func horizonPrice(rec obs.Record, seconds int) float64 {
	switch seconds {
	case 10:
		return rec.PriceAfter10s
	case 60:
		return rec.PriceAfter60s
	case 300:
		return rec.PriceAfter300s
	}
	return 0
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
