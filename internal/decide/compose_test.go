package decide

import (
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

var tickAt = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// healthy is a market with nothing wrong with it, so each test can break
// exactly one thing and attribute the result to that.
func healthy() marketstate.Snapshot {
	return marketstate.Snapshot{
		At:           tickAt,
		Pair:         "xrp_jpy",
		Last:         202.25,
		BestBid:      202.24,
		BestAsk:      202.26,
		SpreadBps:    1.0,
		BookSynced:   true,
		CircuitBreak: "NONE",
	}
}

func long() marketstate.Position {
	return marketstate.Position{Side: "long", Size: 100, EntryPrice: 200, OpenedAt: tickAt.Add(-time.Minute)}
}

// answers builds a full, confident, benign answer set. Options mutate it.
func answers(opts ...func(map[string]jev.Answer)) map[string]jev.Answer {
	a := map[string]jev.Answer{
		jev.QAnomaly:    {Type: "noul", Noul: 0.05},
		jev.QFakeout:    {Type: "noul", Noul: 0.10},
		jev.QHoldRisk:   {Type: "score", Score: 1.0, Confidence: 0.9},
		jev.QEntryScore: {Type: "score", Score: 4.0, Confidence: 0.9},
		jev.QAction: {
			Type:          "choice",
			Choice:        jev.ActionWait,
			Confidence:    0.9,
			Probabilities: map[string]float64{jev.ActionWait: 0.9},
		},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func action(choice string, prob, conf float64) func(map[string]jev.Answer) {
	return func(a map[string]jev.Answer) {
		a[jev.QAction] = jev.Answer{
			Type:          "choice",
			Choice:        choice,
			Confidence:    conf,
			Probabilities: map[string]float64{choice: prob},
		}
	}
}

func score(id string, v float64) func(map[string]jev.Answer) {
	return func(a map[string]jev.Answer) { a[id] = jev.Answer{Type: "score", Score: v, Confidence: 0.9} }
}

func noul(id string, v float64) func(map[string]jev.Answer) {
	return func(a map[string]jev.Answer) { a[id] = jev.Answer{Type: "noul", Noul: v} }
}

func TestCompose(t *testing.T) {
	t.Parallel()
	th := DefaultThresholds()

	cases := []struct {
		name     string
		age      time.Duration
		snap     marketstate.Snapshot
		pos      marketstate.Position
		ans      map[string]jev.Answer
		want     Intent
		wantGate Gate
	}{
		// --- gate 0: freshness -------------------------------------------
		{
			name: "stale answer is not acted on even when it says buy",
			age:  3 * time.Second,
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentNone, wantGate: GateDecisionAge,
		},
		{
			name: "stale answer does not flatten either; the next tick will decide",
			age:  3 * time.Second,
			snap: healthy(), pos: long(),
			ans:  answers(action(jev.ActionStop, 0.9, 0.9)),
			want: IntentNone, wantGate: GateDecisionAge,
		},
		{
			name: "an answer exactly at the bound is still usable",
			age:  th.MaxDecisionAge,
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentOpenLong, wantGate: GateNone,
		},

		// --- gate 1: feed health -----------------------------------------
		{
			name: "unseeded book blocks entry",
			snap: func() marketstate.Snapshot { s := healthy(); s.BookSynced = false; return s }(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentNone, wantGate: GateFeed,
		},
		{
			name: "stale feed flattens an open position",
			snap: func() marketstate.Snapshot { s := healthy(); s.Stale = true; return s }(),
			pos:  long(),
			ans:  answers(action(jev.ActionWait, 0.9, 0.9)),
			want: IntentClose, wantGate: GateFeed,
		},

		// --- gate 2: exchange halt ---------------------------------------
		{
			name: "circuit break blocks entry",
			snap: func() marketstate.Snapshot { s := healthy(); s.CircuitBreak = "CIRCUIT_BREAK"; return s }(),
			ans:  answers(action(jev.ActionBuy, 0.99, 0.99)),
			want: IntentNone, wantGate: GateHalt,
		},
		{
			name: "circuit break flattens an open position",
			snap: func() marketstate.Snapshot { s := healthy(); s.CircuitBreak = "RESUMPTION"; return s }(),
			pos:  long(),
			ans:  answers(),
			want: IntentClose, wantGate: GateHalt,
		},

		// --- gate 3: model-judged anomaly --------------------------------
		{
			name: "anomaly flattens an open position",
			snap: healthy(), pos: long(),
			ans:  answers(noul(jev.QAnomaly, 0.9)),
			want: IntentClose, wantGate: GateAnomaly,
		},
		{
			name: "anomaly blocks a would-be entry",
			snap: healthy(),
			ans:  answers(noul(jev.QAnomaly, 0.9), action(jev.ActionBuy, 0.99, 0.99)),
			want: IntentNone, wantGate: GateAnomaly,
		},
		{
			name: "anomaly exactly at the threshold does not fire",
			snap: healthy(),
			ans:  answers(noul(jev.QAnomaly, th.MaxAnomalyNoul), action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentOpenLong, wantGate: GateNone,
		},

		// --- gate 4: hold risk -------------------------------------------
		{
			name: "hold risk above the floor exits",
			snap: healthy(), pos: long(),
			ans:  answers(score(jev.QHoldRisk, 4.0)),
			want: IntentClose, wantGate: GateHoldRisk,
		},
		{
			name: "hold risk is irrelevant when flat",
			snap: healthy(),
			ans:  answers(score(jev.QHoldRisk, 4.0), action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentOpenLong, wantGate: GateNone,
		},

		// --- gate 5: confidence floors ------------------------------------
		{
			name: "probability below the floor blocks",
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.50, 0.9)),
			want: IntentNone, wantGate: GateConfidence,
		},
		{
			name: "confidence below the floor blocks",
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.50)),
			want: IntentNone, wantGate: GateConfidence,
		},

		// --- entries -------------------------------------------------------
		{
			name: "confident buy on a clean setup opens long",
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.8, 0.8)),
			want: IntentOpenLong, wantGate: GateNone,
		},
		{
			name: "confident sell on a clean setup opens short",
			snap: healthy(),
			ans:  answers(action(jev.ActionSell, 0.8, 0.8)),
			want: IntentOpenShort, wantGate: GateNone,
		},
		{
			name: "no pyramiding onto an existing position",
			snap: healthy(), pos: long(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentNone, wantGate: GatePyramid,
		},
		{
			name: "poor entry timing blocks the entry",
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9), score(jev.QEntryScore, 2.0)),
			want: IntentNone, wantGate: GateEntryQuality,
		},
		{
			name: "fakeout risk blocks the entry",
			snap: healthy(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9), noul(jev.QFakeout, 0.9)),
			want: IntentNone, wantGate: GateFakeout,
		},
		{
			name: "a spread too wide to cross blocks the entry",
			snap: func() marketstate.Snapshot { s := healthy(); s.SpreadBps = 40; return s }(),
			ans:  answers(action(jev.ActionBuy, 0.9, 0.9)),
			want: IntentNone, wantGate: GateSpread,
		},
		{
			name: "a wide spread does NOT trap an open position",
			snap: func() marketstate.Snapshot { s := healthy(); s.SpreadBps = 40; return s }(),
			pos:  long(),
			ans:  answers(action(jev.ActionStop, 0.9, 0.9)),
			want: IntentClose, wantGate: GateNone,
		},

		// --- exits ---------------------------------------------------------
		{
			name: "take profit closes a position",
			snap: healthy(), pos: long(),
			ans:  answers(action(jev.ActionTake, 0.9, 0.9)),
			want: IntentClose, wantGate: GateNone,
		},
		{
			name: "cut loss closes a position",
			snap: healthy(), pos: long(),
			ans:  answers(action(jev.ActionStop, 0.9, 0.9)),
			want: IntentClose, wantGate: GateNone,
		},
		{
			name: "an exit with nothing to exit is a no-op",
			snap: healthy(),
			ans:  answers(action(jev.ActionTake, 0.9, 0.9)),
			want: IntentNone, wantGate: GateFlat,
		},

		// --- wait and degenerate inputs -------------------------------------
		{
			name: "wait does nothing",
			snap: healthy(),
			ans:  answers(action(jev.ActionWait, 0.9, 0.9)),
			want: IntentNone, wantGate: GateWait,
		},
		{
			name: "an unknown choice falls through to wait rather than guessing",
			snap: healthy(),
			ans:  answers(action("something_new", 0.9, 0.9)),
			want: IntentNone, wantGate: GateWait,
		},
		{
			name: "an empty answer set never trades",
			snap: healthy(),
			ans:  map[string]jev.Answer{},
			want: IntentNone, wantGate: GateConfidence,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Compose(tickAt, tickAt.Add(tc.age), tc.snap, tc.ans, tc.pos, th)
			if got.Intent != tc.want {
				t.Errorf("intent = %q, want %q (reason: %s)", got.Intent, tc.want, got.Reason)
			}
			if got.Gate != tc.wantGate {
				t.Errorf("gate = %q, want %q (reason: %s)", got.Gate, tc.wantGate, got.Reason)
			}
			if got.Reason == "" {
				t.Error("every signal must carry a reason for the log")
			}
			if got.Intent == IntentOpenLong || got.Intent == IntentOpenShort {
				if !tc.pos.IsFlat() {
					t.Error("opened a position while already holding one")
				}
			}
		})
	}
}

// A noul answer carries no confidence field (CLAUDE.md rule 4). Reading one
// would yield zero, which must not be mistaken for a low-confidence answer on
// the questions that do have confidence.
func TestComposeIgnoresConfidenceOnNoulAnswers(t *testing.T) {
	t.Parallel()
	ans := answers(action(jev.ActionBuy, 0.9, 0.9))
	ans[jev.QAnomaly] = jev.Answer{Type: "noul", Noul: 0.05} // no Confidence set
	ans[jev.QFakeout] = jev.Answer{Type: "noul", Noul: 0.05}

	got := Compose(tickAt, tickAt, healthy(), ans, marketstate.Position{}, DefaultThresholds())
	if got.Intent != IntentOpenLong {
		t.Fatalf("intent = %q, want %q (reason: %s)", got.Intent, IntentOpenLong, got.Reason)
	}
}

// The brake ordering is the whole point of the design: a blocked state must
// never reach the entry logic, whichever accelerator is screaming.
func TestBrakesBeatAccelerators(t *testing.T) {
	t.Parallel()
	loud := answers(
		action(jev.ActionBuy, 1.0, 1.0),
		score(jev.QEntryScore, 4.0),
		noul(jev.QAnomaly, 1.0), // ...but the market is broken
	)
	got := Compose(tickAt, tickAt, healthy(), loud, marketstate.Position{}, DefaultThresholds())
	if got.Intent != IntentNone || got.Gate != GateAnomaly {
		t.Fatalf("got intent %q gate %q, want none/anomaly", got.Intent, got.Gate)
	}
}

func TestComposeIsPure(t *testing.T) {
	t.Parallel()
	snap, ans, pos, th := healthy(), answers(action(jev.ActionBuy, 0.9, 0.9)), marketstate.Position{}, DefaultThresholds()

	first := Compose(tickAt, tickAt, snap, ans, pos, th)
	for i := 0; i < 10; i++ {
		if got := Compose(tickAt, tickAt, snap, ans, pos, th); got != first {
			t.Fatalf("Compose is not deterministic: %+v != %+v", got, first)
		}
	}
}

func TestSignalRecordsDecisionAge(t *testing.T) {
	t.Parallel()
	got := Compose(tickAt, tickAt.Add(450*time.Millisecond), healthy(), answers(), marketstate.Position{}, DefaultThresholds())
	if got.AgeMs != 450 {
		t.Fatalf("AgeMs = %v, want 450", got.AgeMs)
	}
}
