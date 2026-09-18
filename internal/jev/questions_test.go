package jev

import (
	"encoding/json"
	"strings"
	"testing"
)

// The question ids are the column names of the dataset (CLAUDE.md rule 6).
// Renaming one breaks comparison against every record already logged, so pin
// the set here: adding an id means adding a line, and a rename fails loudly.
func TestQuestionIDsAreStable(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"regime": true, "momentum": true, "volatility": true,
		"fakeout_risk": true, "book_pressure": true,
		"trader_action": true, "entry_quality": true,
		"anomalous": true, "hold_risk": true,
	}
	qs := QuestionSet()
	for id := range qs {
		if !want[id] {
			t.Errorf("unexpected question id %q — appending is fine, renaming breaks history", id)
		}
		delete(want, id)
	}
	for id := range want {
		t.Errorf("question id %q disappeared; removing one loses history", id)
	}
}

func TestDefaultQuestionSetIsValid(t *testing.T) {
	t.Parallel()
	if err := Validate(QuestionSet()); err != nil {
		t.Fatal(err)
	}
}

// CLAUDE.md rule 3: without an escape option the model is forced to pick the
// least-wrong label for a state that fits none of them.
func TestEveryChoiceQuestionHasAnEscapeOption(t *testing.T) {
	t.Parallel()
	for id, q := range QuestionSet() {
		if q.Type != "choice" {
			continue
		}
		escape, ok := escapeOptions[id]
		if !ok {
			t.Errorf("choice question %q declares no escape option", id)
			continue
		}
		criteria, ok := q.Criteria.(map[string]any)
		if !ok {
			t.Errorf("choice question %q has criteria of the wrong shape", id)
			continue
		}
		if _, ok := criteria[escape]; !ok {
			t.Errorf("choice question %q is missing its escape option %q", id, escape)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		qs   map[string]Question
		want string
	}{
		"an empty set": {
			map[string]Question{}, "empty",
		},
		"a question with no instructions": {
			map[string]Question{"x": {Type: "noul"}}, "instructions",
		},
		"an unknown type": {
			map[string]Question{"x": {Type: "ranking", Instructions: "?"}}, "unknown type",
		},
		"a choice with one option": {
			map[string]Question{QAction: {Type: "choice", Instructions: "?", Criteria: map[string]any{"only": nil}}},
			"at least two options",
		},
		"a choice missing its escape option": {
			map[string]Question{QAction: {Type: "choice", Instructions: "?", Criteria: map[string]any{
				ActionBuy: "long", ActionSell: "short",
			}}},
			"escape option",
		},
		"an undeclared choice question": {
			map[string]Question{"new_choice": {Type: "choice", Instructions: "?", Criteria: map[string]any{
				"a": nil, "b": nil,
			}}},
			"escape option",
		},
		"a score with one level": {
			map[string]Question{"x": {Type: "score", Instructions: "?", Criteria: []string{"only"}}},
			"at least two ordered levels",
		},
		"a noul with half its criteria": {
			map[string]Question{"x": {Type: "noul", Instructions: "?", Criteria: map[string]string{"true": "yes"}}},
			`missing "false"`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(tc.qs)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// CLAUDE.md rule 2: nothing code can compute exactly belongs in a question.
// This is a blunt check, but it catches the obvious relapse.
func TestNoQuestionAsksForArithmetic(t *testing.T) {
	t.Parallel()
	banned := []string{"average", "calculate", "compute", "what is the ratio", "how many percent"}
	for id, q := range QuestionSet() {
		lower := strings.ToLower(q.Instructions)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Errorf("question %q asks the model to %q; compute it in marketstate instead", id, word)
			}
		}
	}
}

func TestQuestionSetSerialisesToTheDocumentedShapes(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(QuestionSet())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}

	if _, ok := decoded[QFakeout]["criteria"].(map[string]any); !ok {
		t.Error("noul criteria should serialise as an object with true/false keys")
	}
	if _, ok := decoded[QMomentum]["criteria"].([]any); !ok {
		t.Error("score criteria should serialise as an ordered array")
	}
	if _, ok := decoded[QAction]["criteria"].(map[string]any); !ok {
		t.Error("choice criteria should serialise as an option object")
	}
}

func TestIDsAreSorted(t *testing.T) {
	t.Parallel()
	ids := IDs(QuestionSet())
	if len(ids) != len(QuestionSet()) {
		t.Fatalf("got %d ids, want %d", len(ids), len(QuestionSet()))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ids are not sorted: %v", ids)
		}
	}
}
