package calib

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

var base = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// record builds one tick whose price moved by retBps over the 60s horizon.
func record(i int, ans map[string]jev.Answer, retBps float64) obs.Record {
	const last = 100.0
	at := base.Add(time.Duration(i) * time.Second)
	return obs.Record{
		TickID:        at.Format(time.RFC3339Nano),
		At:            at,
		Snapshot:      marketstate.Snapshot{At: at, Last: last},
		Answers:       ans,
		PriceAfter60s: last * (1 + retBps/10000),
	}
}

func noulAnswer(p float64) map[string]jev.Answer {
	return map[string]jev.Answer{jev.QBookPressure: {Type: "noul", Noul: p}}
}

func directionOpts() Options {
	return Options{QuestionID: jev.QBookPressure, Outcome: OutcomeDirection, HorizonSec: 60, Bins: 10}
}

// A perfectly calibrated forecaster: in each bucket, the stated probability is
// exactly the fraction of cases that come true.
func TestPerfectCalibrationScoresNearZeroError(t *testing.T) {
	t.Parallel()
	var records []obs.Record
	i := 0
	for _, p := range []float64{0.15, 0.35, 0.55, 0.75, 0.95} {
		const n = 200
		ups := int(p * n)
		for k := 0; k < n; k++ {
			ret := -10.0
			if k < ups {
				ret = 10.0
			}
			records = append(records, record(i, noulAnswer(p), ret))
			i++
		}
	}

	rep, err := Build(records, directionOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != len(records) {
		t.Fatalf("usable = %d, want %d (skipped: %v)", rep.Usable, len(records), rep.Skipped)
	}
	if rep.ECE > 0.01 {
		t.Errorf("ECE = %.4f, want ~0 for a perfectly calibrated forecaster", rep.ECE)
	}
	for _, b := range rep.Bins {
		if b.N > 0 && math.Abs(b.Gap()) > 0.02 {
			t.Errorf("bucket %.1f-%.1f: gap %.3f, want ~0", b.Lo, b.Hi, b.Gap())
		}
	}
}

// The failure mode the experiment is looking for: high stated probability,
// coin-flip reality.
func TestOverconfidenceShowsAsAPositiveGap(t *testing.T) {
	t.Parallel()
	var records []obs.Record
	for i := 0; i < 400; i++ {
		ret := -10.0
		if i%2 == 0 {
			ret = 10.0
		}
		records = append(records, record(i, noulAnswer(0.95), ret))
	}

	rep, err := Build(records, directionOpts())
	if err != nil {
		t.Fatal(err)
	}
	last := rep.Bins[len(rep.Bins)-1]
	if last.N != 400 {
		t.Fatalf("top bucket has %d observations, want 400", last.N)
	}
	if math.Abs(last.Realised-0.5) > 0.01 {
		t.Errorf("realised = %.3f, want 0.5", last.Realised)
	}
	if last.Gap() < 0.4 {
		t.Errorf("gap = %.3f, want ~0.45 of overconfidence", last.Gap())
	}
	if rep.ECE < 0.4 {
		t.Errorf("ECE = %.3f, want it to reflect the overconfidence", rep.ECE)
	}
	// Always saying 0.95 about a coin flip scores worse than always saying 0.5.
	if rep.Brier < 0.25 {
		t.Errorf("Brier = %.4f, want worse than the 0.25 of a fair coin call", rep.Brier)
	}
}

func TestActionOutcomeUsesTheBand(t *testing.T) {
	t.Parallel()
	buy := func(p float64) map[string]jev.Answer {
		return map[string]jev.Answer{jev.QAction: {
			Type: "choice", Choice: jev.ActionBuy, Confidence: p,
			Probabilities: map[string]float64{jev.ActionBuy: p},
		}}
	}
	wait := map[string]jev.Answer{jev.QAction: {
		Type: "choice", Choice: jev.ActionWait, Confidence: 0.9,
		Probabilities: map[string]float64{jev.ActionWait: 0.9},
	}}

	opts := Options{QuestionID: jev.QAction, Outcome: OutcomeAction, HorizonSec: 60, BandBps: 24, Bins: 10}

	// A 10bps move does not clear a 24bps round trip, so the buy was wrong and
	// waiting was right — the band is what makes "was this call correct"
	// answerable in fee terms rather than in sign terms.
	rep, err := Build([]obs.Record{record(0, buy(0.9), 10), record(1, wait, 10)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 2 {
		t.Fatalf("usable = %d, want 2 (skipped: %v)", rep.Usable, rep.Skipped)
	}
	if rep.BaseRate != 0.5 {
		t.Errorf("base rate = %v, want 0.5: one right call and one wrong one", rep.BaseRate)
	}

	// The same 10bps move with no band: the buy was right, and the wait is not
	// wrong — it is unanswerable, because "waiting was right" only means
	// anything relative to a band. Scoring it as wrong is what produced a
	// reliability table full of false overconfidence on the first real dataset.
	opts.BandBps = 0
	rep, err = Build([]obs.Record{record(0, buy(0.9), 10), record(1, wait, 10)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 1 {
		t.Fatalf("usable = %d, want 1: the buy is scoreable, the wait is not", rep.Usable)
	}
	if rep.BaseRate != 1 {
		t.Errorf("base rate = %v, want 1: the only scoreable call, the buy, was right", rep.BaseRate)
	}
}

func TestExitActionsAreNotScoredAgainstPriceAlone(t *testing.T) {
	t.Parallel()
	exit := map[string]jev.Answer{jev.QAction: {
		Type: "choice", Choice: jev.ActionTake, Confidence: 0.9,
		Probabilities: map[string]float64{jev.ActionTake: 0.9},
	}}
	rep, err := Build([]obs.Record{record(0, exit, 50)},
		Options{QuestionID: jev.QAction, Outcome: OutcomeAction, HorizonSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 0 {
		t.Errorf("usable = %d, want 0: an exit's correctness depends on a position, not on price", rep.Usable)
	}
	if rep.Skipped["action has no price-derived truth"] != 1 {
		t.Errorf("skip reasons = %v", rep.Skipped)
	}
}

// Every dropped record is counted by reason: a report over a silently biased
// subset would be worse than no report at all.
func TestUnusableRecordsAreCountedByReason(t *testing.T) {
	t.Parallel()
	failed := record(0, noulAnswer(0.9), 10)
	failed.Error = "typesafe 529: overloaded"

	noHorizon := record(1, noulAnswer(0.9), 10)
	noHorizon.PriceAfter60s = 0

	unanswered := record(2, map[string]jev.Answer{}, 10)
	tie := record(3, noulAnswer(0.9), 0)

	rep, err := Build([]obs.Record{failed, noHorizon, unanswered, tie}, directionOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 0 {
		t.Fatalf("usable = %d, want 0", rep.Usable)
	}
	want := map[string]int{
		"failed call":           1,
		"horizon price missing": 1,
		"question not answered": 1,
		"flat horizon (tie)":    1,
	}
	for reason, n := range want {
		if rep.Skipped[reason] != n {
			t.Errorf("skipped[%q] = %d, want %d (all: %v)", reason, rep.Skipped[reason], n, rep.Skipped)
		}
	}
}

func TestProbabilityOfUp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ans  jev.Answer
		want float64
		ok   bool
	}{
		{
			name: "a noul answer is already a probability",
			ans:  jev.Answer{Type: "noul", Noul: 0.72},
			want: 0.72, ok: true,
		},
		{
			name: "a score answer uses the mass above the middle level",
			ans: jev.Answer{Type: "score", Probabilities: map[string]float64{
				"0": 0.1, "1": 0.1, "2": 0.3, "3": 0.3, "4": 0.2,
			}},
			want: 0.5, ok: true, // levels 3 and 4 are the upward half
		},
		{
			name: "a choice answer weighs long against short",
			ans: jev.Answer{Type: "choice", Probabilities: map[string]float64{
				jev.ActionBuy: 0.3, jev.ActionSell: 0.1, jev.ActionWait: 0.6,
			}},
			want: 0.75, ok: true,
		},
		{
			name: "a choice with no directional mass has no reading",
			ans:  jev.Answer{Type: "choice", Probabilities: map[string]float64{jev.ActionWait: 1}},
			ok:   false,
		},
		{
			name: "a score with no distribution has no reading",
			ans:  jev.Answer{Type: "score", Score: 3},
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := probabilityOfUp(tc.ans)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("p(up) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBinEdges(t *testing.T) {
	t.Parallel()
	obsv := []Observation{
		{Predicted: 0.0, Correct: true},
		{Predicted: 1.0, Correct: true}, // must land in the last bucket, not overflow
		{Predicted: 0.999, Correct: false},
	}
	bins := bin(obsv, 10)
	if bins[0].N != 1 {
		t.Errorf("first bucket has %d, want 1", bins[0].N)
	}
	if bins[9].N != 2 {
		t.Errorf("last bucket has %d, want 2", bins[9].N)
	}
	if bins[9].Realised != 0.5 {
		t.Errorf("last bucket realised = %v, want 0.5", bins[9].Realised)
	}
}

func TestBuildRejectsAMissingQuestion(t *testing.T) {
	t.Parallel()
	if _, err := Build(nil, Options{Outcome: OutcomeDirection, HorizonSec: 60}); err == nil {
		t.Fatal("expected an error when no question id is given")
	}
}

func TestUnknownHorizonYieldsNothing(t *testing.T) {
	t.Parallel()
	opts := directionOpts()
	opts.HorizonSec = 45 // not a column in the schema
	rep, err := Build([]obs.Record{record(0, noulAnswer(0.9), 10)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 0 || rep.Skipped["horizon price missing"] != 1 {
		t.Errorf("usable = %d, skipped = %v", rep.Usable, rep.Skipped)
	}
}

func TestPercentile(t *testing.T) {
	t.Parallel()
	xs := []float64{5, 1, 4, 2, 3}
	for p, want := range map[float64]float64{0: 1, 50: 3, 90: 5, 100: 5} {
		if got := Percentile(append([]float64(nil), xs...), p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("percentile of nothing = %v, want 0", got)
	}
}

func TestTallySortsByCountThenName(t *testing.T) {
	t.Parallel()
	tally := Tally{}
	for _, g := range []string{"wait", "wait", "wait", "anomaly", "anomaly", "spread"} {
		tally.Add(g)
	}
	got := fmt.Sprint(tally.Sorted())
	if want := "[wait anomaly spread]"; got != want {
		t.Errorf("Sorted() = %s, want %s", got, want)
	}
}

// The first real dataset produced a top bucket of 114 observations with a
// realised rate of exactly 0.000, which is not a thing a model does. It was
// this: with no band, "waiting was right" reduced to the price being unchanged
// to floating-point equality after sixty seconds, so every high-confidence wait
// — most of what the model says — counted as wrong.
func TestWaitIsNotScoredWrongWhenItCannotBeScoredAtAll(t *testing.T) {
	t.Parallel()
	wait := map[string]jev.Answer{jev.QAction: {
		Type: "choice", Choice: jev.ActionWait, Confidence: 0.96,
		Probabilities: map[string]float64{jev.ActionWait: 0.96},
	}}

	// Any realistic move: the price is never exactly unchanged.
	records := []obs.Record{record(0, wait, 3), record(1, wait, -2), record(2, wait, 0.5)}

	opts := Options{QuestionID: jev.QAction, Outcome: OutcomeAction, HorizonSec: 60, BandBps: 0}
	rep, err := Build(records, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 0 {
		t.Fatalf("usable = %d, want 0: wait has no truth without a band", rep.Usable)
	}
	if rep.Skipped["wait is unscoreable with -band-bps 0"] != 3 {
		t.Errorf("skip reasons = %v, want three unscoreable waits", rep.Skipped)
	}

	// With a band it is a real question again, and these small moves are all
	// inside a 24bps round trip, so waiting was right every time.
	opts.BandBps = 24
	rep, err = Build(records, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usable != 3 {
		t.Fatalf("usable = %d, want 3 once a band exists", rep.Usable)
	}
	if rep.BaseRate != 1 {
		t.Errorf("base rate = %v, want 1: every move was inside the band", rep.BaseRate)
	}
}
