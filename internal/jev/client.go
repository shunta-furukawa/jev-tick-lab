// Package jev is a client for TypeSafe's System One endpoint.
//
//	POST https://api.typesafe.ai/v1/systemone
//	Authorization: Bearer <API_KEY>
//
// Request:  {"state": ..., "model": "...", "questions": {"<id>": Question}}
// Response: {"model": "...", "answers": {"<id>": Answer}, "usage": {...}}
//
// Rate limits at the time of writing: 1,200 requests/min and 250,000 tokens/sec.
// TypeSafe warns these move without notice. A 1s cadence uses 60 rpm, so there
// is plenty of headroom — but back off properly on 429 and 529 anyway.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is TypeSafe's System One endpoint. Client.Endpoint overrides
// it, which is how the tests point the client at a local fake.
const DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"

// Question is one typed question. Criteria's shape depends on Type:
//
//	noul   -> map[string]string with "true" and "false" keys (optional)
//	choice -> map[string]any  option -> description (or nil)
//	score  -> []string        ordered levels, at least two
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type Request struct {
	State     string              `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// Answer is the union of all three answer shapes. Check Type before reading.
//
// Note: Noul answers carry NO confidence field. Code that assumes every answer
// has one will silently misread binary questions.
type Answer struct {
	Type string `json:"type"`

	Noul float64 `json:"noul"` // noul only

	Choice string `json:"choice"` // choice only

	Score  float64           `json:"score"`  // score only
	Legend map[string]string `json:"legend"` // score only

	Probabilities map[string]float64 `json:"probabilities"` // choice + score
	Confidence    float64            `json:"confidence"`    // choice + score
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response.Model is the VERSIONED id that actually answered. Always log it:
// `jev-latest` moves, and a moved model invalidates tuned thresholds.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Client holds the credential, so it is also the only place that can reliably
// keep it out of anything it returns. See redact.
type Client struct {
	APIKey     string
	Model      string // pin a versioned id in production, e.g. "jev-1.13.0"
	HTTP       *http.Client
	MaxRetries int
	Endpoint   string
}

func New(apiKey, model string) *Client {
	return &Client{
		APIKey:     apiKey,
		Model:      model,
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		MaxRetries: 2, // a stale answer is worthless; fail fast and skip the tick
		Endpoint:   DefaultEndpoint,
	}
}

// String keeps the credential out of a `%v` or `%+v` of the client. Logging a
// whole client is not something this code does, but the cost of making it safe
// is one method.
func (c *Client) String() string {
	return fmt.Sprintf("jev.Client{Model: %q, Endpoint: %q, APIKey: [%d chars, redacted]}",
		c.Model, c.Endpoint, len(c.APIKey))
}

// redact removes the credential from a string.
//
// The key is only ever sent as a header, so it should never come back. But an
// error string from a failed call is written into the JSONL tick record, which
// is shipped to GCS and loaded into BigQuery — so if a server ever did echo a
// credential into an error body, the result would be the key sitting in an
// archive forever. The probability is low; the blast radius is permanent.
func (c *Client) redact(s string) string {
	if c.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.APIKey, "[REDACTED]")
}

type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration // from the Retry-After header, when the server sent one
}

func (e *APIError) Error() string { return fmt.Sprintf("typesafe %d: %s", e.Status, e.Body) }

func (e *APIError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == 529 || e.Status >= 500
}

// Retryable reports whether this error is worth another attempt. 401 and 422
// are configuration mistakes: retrying them just burns the tick.
func (e *APIError) Retryable() bool { return e.retryable() }

// Ask evaluates all questions against one state in a single call.
//
// Batching matters: questions are evaluated in parallel and in isolation, so
// adding questions barely changes latency. TypeSafe's own cookbook measured a
// 13-question batch at 12.2x cheaper and 10.0x faster than asking one at a time.
func (c *Client) Ask(ctx context.Context, state string, questions map[string]Question) (*Response, error) {
	body, err := json.Marshal(Request{State: state, Model: c.Model, Questions: questions})
	if err != nil {
		return nil, err
	}

	var (
		lastErr error
		wait    time.Duration
	)
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * 200 * time.Millisecond
			// Jitter: every instance of this process ticks on the same second,
			// so a synchronised retry storm is a real possibility.
			delay += time.Duration(rand.Int63n(int64(delay/2 + 1)))
			if wait > delay {
				delay = wait // the server told us how long to wait
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		target := c.Endpoint
		if target == "" {
			target = DefaultEndpoint
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			// A transport error carries the URL rather than the header, so this
			// is belt-and-braces. It is also the cheap kind.
			lastErr = fmt.Errorf("%s", c.redact(err.Error()))
			continue
		}

		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			apiErr := &APIError{
				Status:     resp.StatusCode,
				Body:       c.redact(string(raw)),
				RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			}
			if !apiErr.retryable() {
				return nil, apiErr
			}
			lastErr, wait = apiErr, apiErr.RetryAfter
			continue
		}

		var out Response
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		return &out, nil
	}
	return nil, fmt.Errorf("exhausted retries: %w", lastErr)
}

// retryAfter reads the header in its delta-seconds form. An absolute HTTP date
// is ignored: at a one-second cadence any wait long enough to be expressed that
// way means the tick is lost anyway.
func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
