package jev

import (
	"fmt"
	"sort"
)

// Question ids. Referenced from decide/ and from the analysis notebooks, so
// treat them as a stable schema: renaming one breaks historical log comparison.
const (
	QRegime       = "regime"
	QMomentum     = "momentum"
	QVolatility   = "volatility"
	QFakeout      = "fakeout_risk"
	QBookPressure = "book_pressure"

	QAction     = "trader_action"
	QEntryScore = "entry_quality"

	QAnomaly  = "anomalous"
	QHoldRisk = "hold_risk"
)

// Choice option values for QAction.
const (
	ActionBuy  = "open_long"
	ActionSell = "open_short"
	ActionWait = "wait"
	ActionTake = "take_profit"
	ActionStop = "cut_loss"
)

// QuestionSet is the batch sent every tick.
//
// Rules for anything added here:
//  1. One judgement per question. If it needs weighing several factors, split it.
//  2. Never ask for something code can compute exactly (an average, a ratio,
//     a breakout level). Compute it and put it in the state instead.
//  3. Every Choice needs an escape option, or the model is forced to pick the
//     least-wrong label for inputs that fit none of them.
//  4. Adding questions is nearly free in latency. Removing one loses history.
func QuestionSet() map[string]Question {
	return map[string]Question{
		QRegime: {
			Type:         "choice",
			Instructions: "What kind of market condition is this right now?",
			Criteria: map[string]any{
				"uptrend":   "Price is making higher highs and higher lows with sustained buying",
				"downtrend": "Price is making lower highs and lower lows with sustained selling",
				"range":     "Price is oscillating within a band with no clear direction",
				"volatile":  "Sharp, erratic moves in both directions; direction is unclear",
				"unclear":   "The data does not support a confident read",
			},
		},
		QMomentum: {
			Type:         "score",
			Instructions: "How strong is the current directional momentum?",
			Criteria: []string{
				"Strongly downward",
				"Mildly downward",
				"Flat or directionless",
				"Mildly upward",
				"Strongly upward",
			},
		},
		QVolatility: {
			Type:         "score",
			Instructions: "How volatile is price action right now relative to a calm market?",
			Criteria:     []string{"Very calm", "Calm", "Normal", "Active", "Violent"},
		},
		QFakeout: {
			Type:         "noul",
			Instructions: "If price has just broken a recent high or low, is that break likely to fail and reverse?",
			Criteria: map[string]string{
				"true":  "The break looks unsupported by volume or book depth and is likely to reverse",
				"false": "The break looks supported, or no break has occurred",
			},
		},
		QBookPressure: {
			Type:         "noul",
			Instructions: "Does the order book and recent trade flow lean toward buyers?",
			Criteria: map[string]string{
				"true":  "Bid depth and taker buying clearly dominate",
				"false": "Ask depth and taker selling clearly dominate, or the two are balanced",
			},
		},

		// The core question: reproduce the snap judgement of an experienced
		// day trader looking at this screen.
		QAction: {
			Type: "choice",
			Instructions: "An experienced short-term day trader is looking at this screen. " +
				"What action do they take in the next few seconds?",
			Criteria: map[string]any{
				ActionBuy:  "Open a new long position now",
				ActionSell: "Open a new short position now",
				ActionWait: "Do nothing; the setup is not there",
				ActionTake: "Close the existing position to lock in profit",
				ActionStop: "Close the existing position to limit a loss",
			},
		},
		QEntryScore: {
			Type:         "score",
			Instructions: "If a trader entered a new position right now, how good would the timing be?",
			Criteria: []string{
				"Clearly bad timing",
				"Poor timing",
				"Neutral",
				"Reasonable timing",
				"Excellent timing",
			},
		},

		// Brake side. These gate everything above.
		QAnomaly: {
			Type:         "noul",
			Instructions: "Is this market state unusual or broken compared to normal trading conditions?",
			Criteria: map[string]string{
				"true":  "Abnormally wide spread, vanished depth, frozen prices, or a violent dislocation",
				"false": "Conditions look ordinary for this market",
			},
		},
		QHoldRisk: {
			Type:         "score",
			Instructions: "How risky is it to keep holding the current position through the next few minutes?",
			Criteria: []string{
				"Very low risk",
				"Low risk",
				"Moderate risk",
				"High risk",
				"Very high risk",
			},
		},
	}
}

// Escape options. Every Choice question must offer one, so the model is never
// forced to pick the least-wrong label for a state that fits none of them
// (CLAUDE.md rule 3). Named here so the check in Validate and the tests agree.
var escapeOptions = map[string]string{
	QRegime: "unclear",
	QAction: ActionWait,
}

// Validate checks the question set against the API's structural rules and this
// project's own. It runs at startup: a malformed set is a 422 on the first
// tick, and the process should say so plainly rather than log a failed call
// once a second forever.
func Validate(qs map[string]Question) error {
	if len(qs) == 0 {
		return fmt.Errorf("question set is empty")
	}
	for id, q := range qs {
		if q.Instructions == "" {
			return fmt.Errorf("question %q: instructions are required", id)
		}
		switch q.Type {
		case "noul":
			// criteria is optional for noul; when present it must be the
			// true/false pair.
			if q.Criteria == nil {
				continue
			}
			c, ok := q.Criteria.(map[string]string)
			if !ok {
				return fmt.Errorf("question %q: noul criteria must be map[string]string", id)
			}
			for _, key := range []string{"true", "false"} {
				if _, ok := c[key]; !ok {
					return fmt.Errorf("question %q: noul criteria is missing %q", id, key)
				}
			}
		case "choice":
			c, ok := q.Criteria.(map[string]any)
			if !ok || len(c) < 2 {
				return fmt.Errorf("question %q: choice needs criteria with at least two options", id)
			}
			escape, ok := escapeOptions[id]
			if !ok {
				return fmt.Errorf("question %q: choice questions must declare an escape option in escapeOptions", id)
			}
			if _, ok := c[escape]; !ok {
				return fmt.Errorf("question %q: missing its escape option %q", id, escape)
			}
		case "score":
			c, ok := q.Criteria.([]string)
			if !ok || len(c) < 2 {
				return fmt.Errorf("question %q: score needs at least two ordered levels", id)
			}
		default:
			return fmt.Errorf("question %q: unknown type %q", id, q.Type)
		}
	}
	return nil
}

// IDs returns the question ids in a stable order, for the run header.
func IDs(qs map[string]Question) []string {
	out := make([]string, 0, len(qs))
	for id := range qs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
