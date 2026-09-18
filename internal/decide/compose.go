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
		MaxDecisionAge:  2 * time.Second,
	}
}

type Intent string

const (
	IntentNone      Intent = "none"
	IntentOpenLong  Intent = "open_long"
	IntentOpenShort Intent = "open_short"
	IntentClose     Intent = "close"
)

// Signal is the output of one evaluation. Reason is free text for the log only —
// nothing downstream branches on it.
type Signal struct {
	At     time.Time
	Intent Intent
	Reason string

	ActionChoice string
	ActionProb   float64
	ActionConf   float64
	EntryQuality float64
	AnomalyNoul  float64
	HoldRisk     float64
}

// Compose applies the gates in order. Brakes are evaluated before accelerators,
// so a blocked state can never reach the entry logic.
func Compose(now time.Time, ans map[string]jev.Answer, pos marketstate.Position, t Thresholds) Signal {
	s := Signal{At: now, Intent: IntentNone}

	if a, ok := ans[jev.QAnomaly]; ok {
		s.AnomalyNoul = a.Noul
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

	// Gate 1: abnormal market. Flatten if holding, never open.
	if s.AnomalyNoul > t.MaxAnomalyNoul {
		if !pos.IsFlat() {
			s.Intent, s.Reason = IntentClose, "anomalous market state"
		} else {
			s.Reason = "anomalous market state; standing aside"
		}
		return s
	}

	// Gate 2: holding something that has become risky.
	if !pos.IsFlat() && s.HoldRisk > t.MaxHoldRisk {
		s.Intent, s.Reason = IntentClose, "hold risk above threshold"
		return s
	}

	// Gate 3: the model must actually be sure.
	if s.ActionProb < t.MinActionProb || s.ActionConf < t.MinConfidence {
		s.Reason = "action below probability or confidence floor"
		return s
	}

	switch s.ActionChoice {
	case jev.ActionTake, jev.ActionStop:
		if !pos.IsFlat() {
			s.Intent, s.Reason = IntentClose, "model calls for exit"
		}
		return s

	case jev.ActionBuy, jev.ActionSell:
		if !pos.IsFlat() {
			s.Reason = "already holding; no pyramiding"
			return s
		}
		if s.EntryQuality < t.MinEntryQuality {
			s.Reason = "entry quality below floor"
			return s
		}
		if a, ok := ans[jev.QFakeout]; ok && a.Noul > t.MaxFakeoutNoul {
			s.Reason = "fakeout risk too high"
			return s
		}
		if s.ActionChoice == jev.ActionBuy {
			s.Intent, s.Reason = IntentOpenLong, "model calls for long entry"
		} else {
			s.Intent, s.Reason = IntentOpenShort, "model calls for short entry"
		}
		return s

	default:
		s.Reason = "model says wait"
		return s
	}
}
