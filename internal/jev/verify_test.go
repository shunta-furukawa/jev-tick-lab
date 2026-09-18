package jev

import (
	"strings"
	"testing"
)

// good is a response that answers the real question set correctly, so each test
// can break exactly one thing.
func good() *Response {
	answers := map[string]Answer{}
	for id, q := range QuestionSet() {
		switch q.Type {
		case "noul":
			answers[id] = Answer{Type: "noul", Noul: 0.2}
		case "choice":
			options, _ := q.Criteria.(map[string]any)
			var pick string
			for k := range options {
				if pick == "" || k < pick {
					pick = k // deterministic
				}
			}
			answers[id] = Answer{
				Type: "choice", Choice: pick, Confidence: 0.8,
				Probabilities: map[string]float64{pick: 0.7, "other": 0.3},
			}
		case "score":
			answers[id] = Answer{
				Type: "score", Score: 2.4, Confidence: 0.6,
				Legend:        map[string]string{"0": "lowest"},
				Probabilities: map[string]float64{"2": 0.6, "3": 0.4},
			}
		}
	}
	return &Response{
		Model:   "jev-1.13.0",
		Answers: answers,
		Usage:   Usage{InputTokens: 1500, OutputTokens: 48},
	}
}

func TestVerifyAcceptsAWellFormedResponse(t *testing.T) {
	t.Parallel()
	problems, notes := Verify(QuestionSet(), "jev-1.13.0", good())
	if len(problems) > 0 {
		t.Errorf("problems on a good response: %v", problems)
	}
	// "other" is not a declared option of any choice question, so the probability
	// map legitimately mentions something the criteria do not. That is not a
	// problem, and must not be reported as one.
	for _, n := range notes {
		t.Logf("note: %s", n)
	}
}

// Rule 5: the whole point of pinning is to notice when it did not hold.
func TestVerifyCatchesADifferentModelAnswering(t *testing.T) {
	t.Parallel()
	resp := good()
	resp.Model = "jev-1.14.0"

	problems, _ := Verify(QuestionSet(), "jev-1.13.0", resp)
	if !containsMatch(problems, "jev-1.14.0") {
		t.Fatalf("problems = %v, want one naming the model that answered", problems)
	}
}

func TestVerifyCatchesAMissingAnswer(t *testing.T) {
	t.Parallel()
	resp := good()
	delete(resp.Answers, QAction)

	problems, _ := Verify(QuestionSet(), "jev-1.13.0", resp)
	if !containsMatch(problems, QAction+": not answered") {
		t.Fatalf("problems = %v", problems)
	}
}

func TestVerifyCatchesBrokenAnswerShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*Response)
		want   string
	}{
		"a noul outside 0..1": {
			func(r *Response) { r.Answers[QAnomaly] = Answer{Type: "noul", Noul: 1.4} },
			"outside 0..1",
		},
		"a choice that was not an option": {
			func(r *Response) {
				r.Answers[QAction] = Answer{Type: "choice", Choice: "moon", Confidence: 0.9,
					Probabilities: map[string]float64{"moon": 1}}
			},
			"not one of the options",
		},
		"a choice with no probabilities": {
			func(r *Response) {
				r.Answers[QAction] = Answer{Type: "choice", Choice: ActionWait, Confidence: 0.9}
			},
			"no probabilities",
		},
		"probabilities that do not sum to 1": {
			func(r *Response) {
				r.Answers[QAction] = Answer{Type: "choice", Choice: ActionWait, Confidence: 0.9,
					Probabilities: map[string]float64{ActionWait: 0.3}}
			},
			"sum to 0.300",
		},
		"confidence out of range": {
			func(r *Response) {
				r.Answers[QAction] = Answer{Type: "choice", Choice: ActionWait, Confidence: 1.7,
					Probabilities: map[string]float64{ActionWait: 1}}
			},
			"confidence 1.7",
		},
		"a score beyond its levels": {
			func(r *Response) {
				r.Answers[QMomentum] = Answer{Type: "score", Score: 9, Confidence: 0.6,
					Probabilities: map[string]float64{"4": 1}, Legend: map[string]string{"0": "x"}}
			},
			"outside 0..4",
		},
		"a score probability keyed outside the levels": {
			func(r *Response) {
				r.Answers[QMomentum] = Answer{Type: "score", Score: 2, Confidence: 0.6,
					Probabilities: map[string]float64{"9": 1}, Legend: map[string]string{"0": "x"}}
			},
			"outside the declared levels",
		},
		"an answer whose type does not match the question": {
			func(r *Response) { r.Answers[QAction] = Answer{Type: "noul", Noul: 0.5} },
			"answered as \"noul\"",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := good()
			tc.mutate(resp)
			problems, _ := Verify(QuestionSet(), "jev-1.13.0", resp)
			if !containsMatch(problems, tc.want) {
				t.Errorf("problems = %v, want one mentioning %q", problems, tc.want)
			}
		})
	}
}

// Rule 4 is an assumption about the API. If it stops holding, say so rather
// than carrying on quietly.
func TestVerifyNotesANoulThatCarriedConfidence(t *testing.T) {
	t.Parallel()
	resp := good()
	resp.Answers[QAnomaly] = Answer{Type: "noul", Noul: 0.2, Confidence: 0.7}

	problems, notes := Verify(QuestionSet(), "jev-1.13.0", resp)
	if len(problems) > 0 {
		t.Errorf("this is a note, not a problem: %v", problems)
	}
	if !containsMatch(notes, "rule 4") {
		t.Errorf("notes = %v", notes)
	}
}

func TestVerifyNotesUnexpectedIDsWithoutFailing(t *testing.T) {
	t.Parallel()
	resp := good()
	resp.Answers["something_new"] = Answer{Type: "noul", Noul: 0.5}

	problems, notes := Verify(QuestionSet(), "jev-1.13.0", resp)
	if len(problems) > 0 {
		t.Errorf("an extra id should not fail a run: %v", problems)
	}
	if !containsMatch(notes, "something_new") {
		t.Errorf("notes = %v", notes)
	}
}

func TestVerifyHandlesNoResponse(t *testing.T) {
	t.Parallel()
	if problems, _ := Verify(QuestionSet(), "jev-1.13.0", nil); len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
}

func containsMatch(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
