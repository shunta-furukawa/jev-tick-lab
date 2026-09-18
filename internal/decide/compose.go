// Package decide turns Jev's answers into a signal.
//
// All weighting lives here, in code, as named constants. That is the whole
// point of the atomic-questions design: when priorities change you edit a
// number in this file, not a prompt. Keep it pure and unit-tested.
package decide

import (
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// Thresholds are deliberately all in one struct so a run can log exactly which
// configuration produced its decisions.
type Thresholds struct {
	MinActionProb   float64 // probability mass required on the chosen action
	MinConfidence   float64 // Choice confidence floor
	MaxAnomalyNoul  float64 // above this, trade nothing
	MaxHoldRisk     float64 // above this, exit any open position
	MinEntryQuality float64 // entry_quality score floor for new positions
	MaxFakeoutNoul  float64 // above this, ignore breakout entries
	MaxSpreadBps    float64 // above this, do not open — the round trip cannot pay for itself
	MaxDecisionAge  time.Duration
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		MinActionProb:   0.55,
		MinConfidence:   0.60,
		MaxAnomalyNoul:  0.30,
		MaxHoldRisk:     3.5, // 0..4 scale
		MinEntryQuality: 3.0,
		MaxFakeoutNoul:  0.60,
		// An all-taker round trip on a JPY alt is 24bps before the spread; a
		// book wider than this is not a market worth crossing. bitbank alt
		// spreads sit well under 5bps in normal conditions, so this only fires
		// when something is wrong.
		MaxSpreadBps:   25.0,
		MaxDecisionAge: 2 * time.Second,
	}
}

type Intent string

const (
	IntentNone      Intent = "none"
	IntentOpenLong  Intent = "open_long"
	IntentOpenShort Intent = "open_short"
	IntentClose     Intent = "close"
)

// Gate names the rule that decided this tick. Reason is free text for a human
// reading the log; Gate is a stable enum, so "how often did each brake fire"
// is a group-by rather than a string match. Treat it like a question id:
// append, never rename.
type Gate string

const (
	GateNone         Gate = ""              // nothing blocked the signal
	GateDecisionAge  Gate = "decision_age"  // the answer arrived too late to use
	GateFeed         Gate = "feed"          // no data, or a book never seeded
	GateHalt         Gate = "halt"          // exchange circuit break
	GateAnomaly      Gate = "anomaly"       // model says the state is broken
	GateHoldRisk     Gate = "hold_risk"     // model says holding has become risky
	GateConfidence   Gate = "confidence"    // below the probability or confidence floor
	GatePyramid      Gate = "pyramid"       // already holding; no adding
	GateEntryQuality Gate = "entry_quality" // timing below the floor
	GateFakeout      Gate = "fakeout"       // breakout looks likely to fail
	GateSpread       Gate = "spread"        // book too wide to cross
	GateWait         Gate = "wait"          // the model simply said wait
	GateFlat         Gate = "flat"          // an exit was called with nothing to exit
)

// Signal is the output of one evaluation. Reason is free text for the log only —
// nothing downstream branches on it.
type Signal struct {
	At     time.Time
	Intent Intent
	Gate   Gate
	Reason string

	AgeMs float64 // how old the market snapshot was when the answer landed

	ActionChoice string
	ActionProb   float64
	ActionConf   float64
	EntryQuality float64
	AnomalyNoul  float64
	FakeoutNoul  float64
	HoldRisk     float64
}

// Compose applies the gates in order. Brakes are evaluated before accelerators,
// so a blocked state can never reach the entry logic.
//
// tickAt is when the snapshot was taken; decidedAt is when the answer came back.
// Both are parameters rather than time.Now() calls so this stays pure: the same
// inputs always produce the same signal, in production and in a test.
func Compose(
	tickAt, decidedAt time.Time,
	snap marketstate.Snapshot,
	ans map[string]jev.Answer,
	pos marketstate.Position,
	t Thresholds,
) Signal {
	s := Signal{At: tickAt, Intent: IntentNone, Gate: GateNone}
	s.AgeMs = float64(decidedAt.Sub(tickAt).Milliseconds())

	// Answers are read defensively: a question that was not answered leaves its
	// field at zero rather than blocking the whole tick. Note that noul answers
	// carry no confidence field — reading one would silently yield zero.
	if a, ok := ans[jev.QAnomaly]; ok {
		s.AnomalyNoul = a.Noul
	}
	if a, ok := ans[jev.QFakeout]; ok {
		s.FakeoutNoul = a.Noul
	}
	if a, ok := ans[jev.QHoldRisk]; ok {
		s.HoldRisk = a.Score
	}
	if a, ok := ans[jev.QEntryScore]; ok {
		s.EntryQuality = a.Score
	}
	if a, ok := ans[jev.QAction]; ok {
		s.ActionChoice = a.Choice
		s.ActionConf = a.Confidence
		s.ActionProb = a.Probabilities[a.Choice]
	}

	// Gate 0: freshness. An answer about a market that is two seconds gone
	// describes a market that no longer exists. Do nothing rather than act
	// late — the next tick is 1s away, and it will have current data.
	if t.MaxDecisionAge > 0 && decidedAt.Sub(tickAt) > t.MaxDecisionAge {
		s.Gate, s.Reason = GateDecisionAge, "answer older than the freshness bound"
		return s
	}

	// Gate 1: can we see the market at all? These are facts off the wire, not
	// judgements, so they are checked before anything the model said. Holding a
	// position into a blind spot is the one thing worse than missing a trade.
	if snap.Stale || !snap.BookSynced {
		return brake(s, pos, GateFeed, "market data is stale or the book is unseeded")
	}

	// Gate 2: the exchange itself says this market is not trading normally.
	if snap.Halted() {
		return brake(s, pos, GateHalt, "exchange circuit break: "+snap.CircuitBreak)
	}

	// Gate 3: abnormal market, as judged by the model. Flatten if holding,
	// never open.
	if s.AnomalyNoul > t.MaxAnomalyNoul {
		return brake(s, pos, GateAnomaly, "anomalous market state")
	}

	// Gate 4: holding something that has become risky.
	if !pos.IsFlat() && s.HoldRisk > t.MaxHoldRisk {
		s.Intent, s.Gate, s.Reason = IntentClose, GateHoldRisk, "hold risk above threshold"
		return s
	}

	// Gate 5: the model must actually be sure.
	if s.ActionProb < t.MinActionProb || s.ActionConf < t.MinConfidence {
		s.Gate, s.Reason = GateConfidence, "action below probability or confidence floor"
		return s
	}

	switch s.ActionChoice {
	case jev.ActionTake, jev.ActionStop:
		if pos.IsFlat() {
			s.Gate, s.Reason = GateFlat, "exit called with no position"
			return s
		}
		s.Intent, s.Reason = IntentClose, "model calls for exit"
		return s

	case jev.ActionBuy, jev.ActionSell:
		if !pos.IsFlat() {
			s.Gate, s.Reason = GatePyramid, "already holding; no pyramiding"
			return s
		}
		if s.EntryQuality < t.MinEntryQuality {
			s.Gate, s.Reason = GateEntryQuality, "entry quality below floor"
			return s
		}
		if s.FakeoutNoul > t.MaxFakeoutNoul {
			s.Gate, s.Reason = GateFakeout, "fakeout risk too high"
			return s
		}
		// The spread is the first cost of the round trip and code knows it
		// exactly, so it is checked here rather than asked about.
		if t.MaxSpreadBps > 0 && snap.SpreadBps > t.MaxSpreadBps {
			s.Gate, s.Reason = GateSpread, "spread too wide to cross"
			return s
		}
		if s.ActionChoice == jev.ActionBuy {
			s.Intent, s.Reason = IntentOpenLong, "model calls for long entry"
		} else {
			s.Intent, s.Reason = IntentOpenShort, "model calls for short entry"
		}
		return s

	default:
		s.Gate, s.Reason = GateWait, "model says wait"
		return s
	}
}

// brake is the shared shape of the hard stops: flatten if there is something to
// flatten, otherwise stand aside. It never opens anything.
func brake(s Signal, pos marketstate.Position, g Gate, reason string) Signal {
	s.Gate = g
	if pos.IsFlat() {
		s.Reason = reason + "; standing aside"
		return s
	}
	s.Intent, s.Reason = IntentClose, reason+"; flattening"
	return s
}
