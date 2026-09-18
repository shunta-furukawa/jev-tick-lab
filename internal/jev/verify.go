package jev

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Verify checks a live response against the questions that produced it.
//
// Validate, next door, checks the question set before it is sent. This checks
// what came back — which is the only way to learn things that no local check
// can know: that the API accepts this question set at all, that it answers
// every question, that the answer shapes are what the documentation says, and
// that the model which actually answered is the one that was pinned.
//
// Problems are things that would invalidate a run. Notes are things a human
// should look at but which do not stop a collection.
func Verify(questions map[string]Question, requestedModel string, resp *Response) (problems, notes []string) {
	if resp == nil {
		return []string{"no response"}, nil
	}

	// Rule 5: always log the version that actually answered, and care when it
	// is not the one asked for — a moved model invalidates tuned thresholds.
	switch {
	case resp.Model == "":
		problems = append(problems, "response carries no model id; a recorded run cannot say what answered it")
	case resp.Model != requestedModel:
		problems = append(problems, fmt.Sprintf("asked for %q but %q answered — thresholds tuned against one do not transfer to the other", requestedModel, resp.Model))
	}

	for _, id := range IDs(questions) {
		q := questions[id]
		a, ok := resp.Answers[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: not answered", id))
			continue
		}
		if a.Type != "" && a.Type != q.Type {
			problems = append(problems, fmt.Sprintf("%s: asked as %q, answered as %q", id, q.Type, a.Type))
			continue
		}
		p, n := verifyAnswer(id, q, a)
		problems = append(problems, p...)
		notes = append(notes, n...)
	}

	// Extra ids are not fatal — the batch still answered what was asked — but
	// they mean the response schema has moved.
	var extra []string
	for id := range resp.Answers {
		if _, asked := questions[id]; !asked {
			extra = append(extra, id)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		notes = append(notes, fmt.Sprintf("answers include ids that were not asked for: %v", extra))
	}

	if resp.Usage.InputTokens <= 0 {
		notes = append(notes, "usage reports no input tokens, so cost cannot be measured from this call")
	}
	return problems, notes
}

func verifyAnswer(id string, q Question, a Answer) (problems, notes []string) {
	switch q.Type {
	case "noul":
		if a.Noul < 0 || a.Noul > 1 {
			problems = append(problems, fmt.Sprintf("%s: noul %v is outside 0..1", id, a.Noul))
		}
		// Rule 4. If this ever stops being true, code that reads confidence
		// defensively is fine, but the rule itself needs rewriting.
		if a.Confidence != 0 {
			notes = append(notes, fmt.Sprintf("%s: a noul answer carried confidence %v; CLAUDE.md rule 4 says it should not", id, a.Confidence))
		}

	case "choice":
		options, _ := q.Criteria.(map[string]any)
		if a.Choice == "" {
			problems = append(problems, fmt.Sprintf("%s: no choice returned", id))
			break
		}
		if _, ok := options[a.Choice]; !ok && len(options) > 0 {
			problems = append(problems, fmt.Sprintf("%s: chose %q, which was not one of the options", id, a.Choice))
		}
		if len(a.Probabilities) == 0 {
			problems = append(problems, fmt.Sprintf("%s: choice answer has no probabilities; decide reads the chosen option's mass", id))
			break
		}
		if _, ok := a.Probabilities[a.Choice]; !ok {
			problems = append(problems, fmt.Sprintf("%s: probabilities do not include the chosen option %q", id, a.Choice))
		}
		problems = append(problems, checkDistribution(id, a.Probabilities)...)
		problems = append(problems, checkConfidence(id, a.Confidence)...)

	case "score":
		levels, _ := q.Criteria.([]string)
		if len(levels) > 0 && (a.Score < 0 || a.Score > float64(len(levels)-1)) {
			problems = append(problems, fmt.Sprintf("%s: score %v is outside 0..%d", id, a.Score, len(levels)-1))
		}
		if len(a.Probabilities) == 0 {
			// calib maps a score onto a direction using its distribution.
			notes = append(notes, fmt.Sprintf("%s: score answer has no probabilities, so only the point value is usable", id))
		} else {
			problems = append(problems, checkDistribution(id, a.Probabilities)...)
			for key := range a.Probabilities {
				level, err := strconv.Atoi(key)
				if err != nil {
					notes = append(notes, fmt.Sprintf("%s: probability key %q is not a level index", id, key))
					continue
				}
				if len(levels) > 0 && (level < 0 || level >= len(levels)) {
					problems = append(problems, fmt.Sprintf("%s: probability key %q is outside the declared levels", id, key))
				}
			}
		}
		if len(a.Legend) == 0 {
			notes = append(notes, fmt.Sprintf("%s: score answer has no legend", id))
		}
		problems = append(problems, checkConfidence(id, a.Confidence)...)
	}
	return problems, notes
}

func checkDistribution(id string, p map[string]float64) []string {
	var problems []string
	var sum float64
	for option, v := range p {
		if v < 0 || v > 1 {
			problems = append(problems, fmt.Sprintf("%s: probability for %q is %v, outside 0..1", id, option, v))
		}
		sum += v
	}
	// Loose on purpose: a false failure here would block a run for nothing.
	if math.Abs(sum-1) > 0.05 {
		problems = append(problems, fmt.Sprintf("%s: probabilities sum to %.3f, not 1", id, sum))
	}
	return problems
}

func checkConfidence(id string, c float64) []string {
	if c <= 0 || c > 1 {
		return []string{fmt.Sprintf("%s: confidence %v is outside 0..1; every gate in decide reads it", id, c)}
	}
	return nil
}
