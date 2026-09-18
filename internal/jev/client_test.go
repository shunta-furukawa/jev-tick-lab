package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := New("test-key", "jev-1.13.0")
	c.Endpoint = srv.URL
	return c
}

func TestAskSendsTheDocumentedRequest(t *testing.T) {
	t.Parallel()
	var got Request
	var auth, contentType string

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth, contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{},"usage":{"input_tokens":1,"output_tokens":2}}`))
	})

	if _, err := c.Ask(context.Background(), "state text", QuestionSet()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", auth)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if got.State != "state text" {
		t.Errorf("state = %q", got.State)
	}
	if got.Model != "jev-1.13.0" {
		t.Errorf("model = %q; a recorded run must pin a version", got.Model)
	}
	if len(got.Questions) != len(QuestionSet()) {
		t.Errorf("sent %d questions, want %d — the batch is what makes this cheap", len(got.Questions), len(QuestionSet()))
	}
}

func TestAskDecodesEachAnswerShape(t *testing.T) {
	t.Parallel()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"model":"jev-1.13.0",
			"answers":{
				"anomalous":{"type":"noul","noul":0.92},
				"trader_action":{"type":"choice","choice":"wait","confidence":0.71,
					"probabilities":{"wait":0.62,"open_long":0.38}},
				"momentum":{"type":"score","score":2.4,"confidence":0.55,
					"legend":{"0":"Strongly downward"},"probabilities":{"2":0.6,"3":0.4}}
			},
			"usage":{"input_tokens":1500,"output_tokens":48}}`))
	})

	resp, err := c.Ask(context.Background(), "state", QuestionSet())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "jev-1.13.0" {
		t.Errorf("model = %q; the versioned id that answered must be logged", resp.Model)
	}
	if resp.Usage.InputTokens != 1500 || resp.Usage.OutputTokens != 48 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// A noul answer carries no confidence field. Reading one yields zero, which
	// is exactly the silent misread CLAUDE.md rule 4 warns about.
	if a := resp.Answers[QAnomaly]; a.Noul != 0.92 || a.Confidence != 0 {
		t.Errorf("noul answer = %+v", a)
	}
	if a := resp.Answers[QAction]; a.Choice != "wait" || a.Confidence != 0.71 || a.Probabilities["wait"] != 0.62 {
		t.Errorf("choice answer = %+v", a)
	}
	if a := resp.Answers[QMomentum]; a.Score != 2.4 || a.Legend["0"] != "Strongly downward" {
		t.Errorf("score answer = %+v", a)
	}
}

func TestAskRetriesOverloadThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(529) // TypeSafe's overloaded status
			return
		}
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{},"usage":{}}`))
	})

	if _, err := c.Ask(context.Background(), "state", QuestionSet()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d calls, want 2", got)
	}
}

func TestAskDoesNotRetryConfigurationErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, 422} {
		var calls atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"bad"}`))
		})

		_, err := c.Ask(context.Background(), "state", QuestionSet())
		if err == nil {
			t.Fatalf("%d: expected an error", status)
		}
		var apiErr *APIError
		if !errorsAs(err, &apiErr) || apiErr.Status != status {
			t.Fatalf("%d: got %v, want an *APIError with that status", status, err)
		}
		if apiErr.Retryable() {
			t.Errorf("%d must not be retried; it is a configuration mistake", status)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("%d: made %d calls, want 1", status, got)
		}
	}
}

func TestAskGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.MaxRetries = 1

	if _, err := c.Ask(context.Background(), "state", QuestionSet()); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "exhausted retries") {
		t.Errorf("error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d calls, want 2 (the first attempt plus one retry)", got)
	}
}

// A stale answer is worthless, so the deadline must win over the retry loop.
func TestAskHonoursTheDeadline(t *testing.T) {
	t.Parallel()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Ask(ctx, "state", QuestionSet()); err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Ask took %v; it should abandon the call at the deadline", elapsed)
	}
}

func TestAskRejectsAMalformedBody(t *testing.T) {
	t.Parallel()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":`))
	})
	if _, err := c.Ask(context.Background(), "state", QuestionSet()); err == nil {
		t.Fatal("a truncated response must be an error, not an empty answer set")
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"":                              0,
		"3":                             3 * time.Second,
		"0":                             0,
		"-1":                            0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, // the absolute form is not useful at a 1s cadence
	}
	for in, want := range cases {
		if got := retryAfter(in); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAPIErrorMessageIncludesTheBody(t *testing.T) {
	t.Parallel()
	err := &APIError{Status: 422, Body: `{"error":"criteria required for choice"}`}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "criteria required") {
		t.Errorf("error = %q; a 422 names the offending field and the log must keep it", err.Error())
	}
}

// errorsAs avoids importing errors just for one call site in tests.
func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// The error string from a failed call is written into the JSONL tick record,
// which is shipped to GCS and loaded into BigQuery. If a credential ever
// reached it, the key would sit in an archive indefinitely.
func TestAFailedCallCannotLeakTheCredentialIntoAnError(t *testing.T) {
	t.Parallel()
	const key = "sk-live-do-not-log-this-0123456789"

	// A server that echoes the credential back in its error body. Nothing says
	// TypeSafe does this; the point is that the record is permanent if it ever
	// happens.
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"invalid key","received":%q}`, r.Header.Get("Authorization"))
	})
	c.APIKey = key

	_, err := c.Ask(context.Background(), "state", QuestionSet())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the credential survived into the error string: %s", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("expected the redaction marker, got: %s", err)
	}
	// The useful part of a 401 or 422 body must survive redaction.
	if !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("redaction ate the diagnostic: %s", err)
	}
}

func TestATransportErrorIsRedactedToo(t *testing.T) {
	t.Parallel()
	const key = "sk-live-secret"

	c := New(key, "jev-1.13.0")
	// A URL containing the key is not how this client works, but it is how a
	// future one might, and the redaction should not care.
	c.Endpoint = "http://127.0.0.1:1/" + key
	c.MaxRetries = 0

	_, err := c.Ask(context.Background(), "state", QuestionSet())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the credential survived into a transport error: %s", err)
	}
}

func TestPrintingTheClientDoesNotPrintTheKey(t *testing.T) {
	t.Parallel()
	const key = "sk-live-secret"
	c := New(key, "jev-1.13.0")

	for _, format := range []string{"%v", "%+v", "%s"} {
		if got := fmt.Sprintf(format, c); strings.Contains(got, key) {
			t.Errorf("%s printed the key: %s", format, got)
		}
	}
}
