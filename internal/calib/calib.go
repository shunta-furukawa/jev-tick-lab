// Package calib turns filled tick records into a calibration report.
//
// The experiment's headline question is not whether Jev makes money. It is
// whether its stated confidence means anything on a task it was never trained
// for: when it says 0.8, does that happen 80% of the time?
//
// Everything here is pure. The command in cmd/calib reads files and prints; the
// arithmetic lives here so it can be tested against fixtures.
package calib

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

// Outcome is how a record's ground truth is derived from the forward price.
type Outcome string

const (
	// OutcomeDirection scores a prediction of "price is higher at the horizon".
	// Ties are dropped rather than counted as either side.
	OutcomeDirection Outcome = "direction"

	// OutcomeAction scores whether the action the model chose was the right
	// call: a long is right if the market rose by more than the band, a short
	// if it fell by more, and waiting is right if it did neither.
	OutcomeAction Outcome = "action"
)

// Options configure one report.
type Options struct {
	QuestionID string
	Outcome    Outcome
	HorizonSec int
	BandBps    float64 // dead zone around zero; 0 scores the raw sign
	Bins       int
}

// Observation is one usable (prediction, truth) pair.
type Observation struct {
	TickID     string
	Predicted  float64 // the model's stated probability that Correct is true
	Correct    bool
	ReturnBps  float64
	Choice     string
	Confidence float64
}

// Bin is one row of the reliability table.
type Bin struct {
	Lo, Hi        float64
	N             int
	MeanPredicted float64
	Realised      float64 // the fraction that actually came true
}

// Gap is the calibration error of this bin: positive means overconfident.
func (b Bin) Gap() float64 { return b.MeanPredicted - b.Realised }

// Report is everything the writeup needs for one question at one horizon.
type Report struct {
	Options  Options
	Total    int // records read
	Usable   int // records that produced an observation
	Skipped  map[string]int
	Bins     []Bin
	Brier    float64
	ECE      float64 // expected calibration error, weighted by bin population
	BaseRate float64 // how often the event happened at all
}

// Build walks the records and produces the report. Records that cannot produce
// an observation — a failed call, a missing horizon price, an unanswered
// question — are counted by reason rather than silently dropped, because a
// report over a biased subset would be worse than no report.
func Build(records []obs.Record, opt Options) (Report, error) {
	if opt.Bins <= 0 {
		opt.Bins = 10
	}
	if opt.QuestionID == "" {
		return Report{}, fmt.Errorf("a question id is required")
	}
	rep := Report{Options: opt, Total: len(records), Skipped: map[string]int{}}

	var obsv []Observation
	for _, rec := range records {
		o, reason := observe(rec, opt)
		if reason != "" {
			rep.Skipped[reason]++
			continue
		}
		obsv = append(obsv, o)
	}
	rep.Usable = len(obsv)
	if rep.Usable == 0 {
		return rep, nil
	}

	var correct int
	for _, o := range obsv {
		if o.Correct {
			correct++
		}
		rep.Brier += (o.Predicted - boolToFloat(o.Correct)) * (o.Predicted - boolToFloat(o.Correct))
	}
	rep.Brier /= float64(len(obsv))
	rep.BaseRate = float64(correct) / float64(len(obsv))
	rep.Bins = bin(obsv, opt.Bins)

	for _, b := range rep.Bins {
		rep.ECE += float64(b.N) / float64(len(obsv)) * math.Abs(b.Gap())
	}
	return rep, nil
}

// observe derives one (prediction, truth) pair, or the reason it cannot.
func observe(rec obs.Record, opt Options) (Observation, string) {
	if rec.Error != "" {
		return Observation{}, "failed call"
	}
	ans, ok := rec.Answers[opt.QuestionID]
	if !ok {
		return Observation{}, "question not answered"
	}
	future, ok := horizonPrice(rec, opt.HorizonSec)
	if !ok {
		return Observation{}, "horizon price missing"
	}
	if rec.Snapshot.Last <= 0 {
		return Observation{}, "no reference price"
	}

	retBps := (future - rec.Snapshot.Last) / rec.Snapshot.Last * 10000
	o := Observation{TickID: rec.TickID, ReturnBps: retBps, Choice: ans.Choice, Confidence: ans.Confidence}

	switch opt.Outcome {
	case OutcomeAction:
		if ans.Type != "choice" || ans.Choice == "" {
			return Observation{}, "not a choice answer"
		}
		correct, ok := actionWasRight(ans.Choice, retBps, opt.BandBps)
		if !ok {
			if ans.Choice == jev.ActionWait {
				return Observation{}, "wait is unscoreable with -band-bps 0"
			}
			// take_profit and cut_loss depend on a position that shadow mode
			// never has; scoring them against price alone would be meaningless.
			return Observation{}, "action has no price-derived truth"
		}
		// The model's stated probability that this particular call is right.
		p, ok := ans.Probabilities[ans.Choice]
		if !ok {
			p = ans.Confidence
		}
		o.Predicted, o.Correct = p, correct
		return o, ""

	case OutcomeDirection, "":
		p, ok := probabilityOfUp(ans)
		if !ok {
			return Observation{}, "no directional reading for this answer"
		}
		if retBps == 0 {
			return Observation{}, "flat horizon (tie)"
		}
		o.Predicted, o.Correct = p, retBps > 0
		return o, ""

	default:
		return Observation{}, "unknown outcome mode"
	}
}

// actionWasRight scores a directional call against the realised move. ok is
// false when the call has no price-derived truth at all.
func actionWasRight(choice string, retBps, bandBps float64) (correct, ok bool) {
	switch choice {
	case jev.ActionBuy:
		return retBps > bandBps, true
	case jev.ActionSell:
		return retBps < -bandBps, true

	case jev.ActionWait:
		// "Waiting was right" means the market did not move enough to be worth
		// trading, which is a statement about a band. With no band it reduces
		// to |return| <= 0 — the price unchanged to floating-point equality
		// after sixty seconds — which is never true.
		//
		// Scored that way, every high-confidence wait counts as wrong, and
		// since wait is most of what the model says, the reliability table
		// fills with false overconfidence. The first real dataset showed a top
		// bucket of 114 observations with a realised rate of exactly 0.000,
		// which is what sent us looking.
		if bandBps <= 0 {
			return false, false
		}
		return math.Abs(retBps) <= bandBps, true

	default:
		return false, false
	}
}

// probabilityOfUp maps any answer shape onto "the model's probability that
// price will be higher at the horizon".
//
//   - noul: the stated probability directly. This is only meaningful for a
//     question whose "true" side means upward pressure, such as book_pressure.
//   - score: the probability mass above the middle level, for an ordered scale
//     that runs from downward to upward, such as momentum.
//   - choice: long against short, renormalised, ignoring the other options.
func probabilityOfUp(a jev.Answer) (float64, bool) {
	switch a.Type {
	case "noul":
		return a.Noul, true

	case "score":
		if len(a.Probabilities) == 0 {
			return 0, false
		}
		levels := make([]int, 0, len(a.Probabilities))
		for k := range a.Probabilities {
			n, err := strconv.Atoi(k)
			if err != nil {
				return 0, false
			}
			levels = append(levels, n)
		}
		sort.Ints(levels)
		mid := float64(levels[len(levels)-1]) / 2
		var up, total float64
		for _, n := range levels {
			p := a.Probabilities[strconv.Itoa(n)]
			total += p
			if float64(n) > mid {
				up += p
			}
		}
		if total <= 0 {
			return 0, false
		}
		return up / total, true

	case "choice":
		long, okLong := a.Probabilities[jev.ActionBuy]
		short, okShort := a.Probabilities[jev.ActionSell]
		if !okLong && !okShort {
			return 0, false
		}
		if long+short <= 0 {
			return 0, false
		}
		return long / (long + short), true

	default:
		return 0, false
	}
}

func horizonPrice(rec obs.Record, seconds int) (float64, bool) {
	var p float64
	switch seconds {
	case 10:
		p = rec.PriceAfter10s
	case 60:
		p = rec.PriceAfter60s
	case 300:
		p = rec.PriceAfter300s
	default:
		return 0, false
	}
	return p, p > 0
}

// bin groups observations into equal-width probability buckets. Empty buckets
// are kept: a gap in the middle of the reliability table is informative.
func bin(obsv []Observation, n int) []Bin {
	bins := make([]Bin, n)
	width := 1.0 / float64(n)
	for i := range bins {
		bins[i].Lo = float64(i) * width
		bins[i].Hi = float64(i+1) * width
	}

	sums := make([]float64, n)
	hits := make([]int, n)
	for _, o := range obsv {
		i := int(o.Predicted / width)
		if i >= n { // a probability of exactly 1 belongs in the last bucket
			i = n - 1
		}
		if i < 0 {
			i = 0
		}
		bins[i].N++
		sums[i] += o.Predicted
		if o.Correct {
			hits[i]++
		}
	}
	for i := range bins {
		if bins[i].N == 0 {
			continue
		}
		bins[i].MeanPredicted = sums[i] / float64(bins[i].N)
		bins[i].Realised = float64(hits[i]) / float64(bins[i].N)
	}
	return bins
}

// Tally counts occurrences of a string key, for the gate and intent histograms.
type Tally map[string]int

func (t Tally) Add(key string) { t[key]++ }

// Sorted returns the keys by descending count, then by name, so two runs of the
// same data print identically.
func (t Tally) Sorted() []string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if t[keys[i]] != t[keys[j]] {
			return t[keys[i]] > t[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// Percentile returns the p-th percentile (0..100) of xs using nearest-rank.
// xs is sorted in place.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	rank := int(math.Ceil(p/100*float64(len(xs)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(xs) {
		rank = len(xs) - 1
	}
	return xs[rank]
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
