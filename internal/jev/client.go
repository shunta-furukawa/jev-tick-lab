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
	"net/http"
	"time"
)

const endpoint = "https://api.typesafe.ai/v1/systemone"

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

type Client struct {
	APIKey     string
	Model      string // pin a versioned id in production, e.g. "jev-1.13.0"
	HTTP       *http.Client
	MaxRetries int
}

func New(apiKey, model string) *Client {
	return &Client{
		APIKey:     apiKey,
		Model:      model,
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		MaxRetries: 2, // a stale answer is worthless; fail fast and skip the tick
	}
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("typesafe %d: %s", e.Status, e.Body) }

func (e *APIError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == 529 || e.Status >= 500
}

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

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * 200 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			apiErr := &APIError{Status: resp.StatusCode, Body: string(raw)}
			if !apiErr.retryable() {
				return nil, apiErr
			}
			lastErr = apiErr
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
