package bitbank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const DefaultEndpoint = "https://api.bitbank.cc/v1"

// Client talks to the bitbank private REST API.
//
// There is deliberately no Withdraw method, and there never should be. See the
// package comment.
type Client struct {
	key    string
	secret string

	Endpoint string
	HTTP     *http.Client

	// TimeWindow is how long a signed request stays valid. The vendor
	// recommends staying under 5s and caps it at 60s. Short is safer: a signed
	// request that leaks is only replayable for this long.
	TimeWindow time.Duration

	// now is swappable so the signature can be tested deterministically.
	now func() time.Time
}

func New(key, secret string) *Client {
	return &Client{
		key:      key,
		secret:   secret,
		Endpoint: DefaultEndpoint,
		// Orders are time-critical but not retried blindly: a timeout on a
		// POST is ambiguous, not a failure. See Order.
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		TimeWindow: 3 * time.Second,
		now:        func() time.Time { return time.Now() },
	}
}

// String exists so that printing a Client cannot print the credential. Without
// it, a single %v in a debug line puts the secret in the journal.
func (c *Client) String() string {
	return fmt.Sprintf("bitbank.Client{Endpoint: %q, Key: [%d chars, redacted], Secret: [%d chars, redacted]}",
		c.Endpoint, len(c.key), len(c.secret))
}

// HasCredentials reports whether this client could authenticate at all.
func (c *Client) HasCredentials() bool { return c.key != "" && c.secret != "" }

// APIError is a bitbank error code. The codes are stable and documented, so
// callers branch on Code rather than on message text.
type APIError struct {
	Code   int
	Status int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("bitbank error %d (http %d): %s", e.Code, e.Status, codeMeaning(e.Code))
}

// Retryable reports whether the same request can safely be sent again.
//
// Deliberately conservative: it covers only transport-level conditions where
// the exchange plainly did not act. Anything about the order itself — rejected
// size, insufficient funds, suspended pair — is a decision, not a blip, and
// retrying it is how a bot places the same order five times.
func (e *APIError) Retryable() bool {
	switch e.Code {
	case 10001, 10003, 10005, 20001:
		return true
	}
	return e.Status >= 500
}

func codeMeaning(code int) string {
	switch code {
	case 10000:
		return "url not found"
	case 10002:
		return "malformed request"
	case 10007:
		return "system maintenance"
	case 20001, 20002, 20003, 20004, 20005:
		return "authentication failed — check the key, the secret, and the clock"
	case 20011:
		return "two-step authentication required"
	case 30101:
		return "missing required parameter"
	case 40020:
		return "order amount below the pair minimum"
	case 50008, 50009:
		return "order not found or already finished"
	case 50061:
		return "insufficient funds"
	case 60001:
		return "insufficient balance"
	case 70009, 70020:
		return "orders restricted right now — the pair or the venue is not trading normally"
	}
	return "see bitbank-api-docs/errors.md"
}

type envelope struct {
	Success int             `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// get issues a signed GET. The signature covers the path including /v1 and any
// query string, exactly as sent.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	rel := path
	if len(q) > 0 {
		rel += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+rel, nil)
	if err != nil {
		return c.redactErr(err)
	}
	// The signed payload is the path as the exchange sees it, which includes
	// the /v1 prefix that Endpoint already carries.
	c.authorize(req, "/v1"+rel)
	return c.do(req, out)
}

// post issues a signed POST.
//
// body is marshalled once; the same bytes are signed and sent. Re-marshalling
// between the two would produce a signature for a body nobody transmitted,
// because Go's map ordering and spacing need not match byte for byte.
func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return c.redactErr(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return c.redactErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req, string(raw))
	return c.do(req, out)
}

func (c *Client) authorize(req *http.Request, payload string) {
	at := c.now().UnixMilli()
	window := c.TimeWindow.Milliseconds()
	if window <= 0 {
		window = 3000
	}
	req.Header.Set("ACCESS-KEY", c.key)
	req.Header.Set("ACCESS-REQUEST-TIME", strconv.FormatInt(at, 10))
	req.Header.Set("ACCESS-TIME-WINDOW", strconv.FormatInt(window, 10))
	req.Header.Set("ACCESS-SIGNATURE", signTimeWindow(c.secret, at, window, payload))
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.redactErr(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.redactErr(err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("bitbank http %d: unparseable response: %s", resp.StatusCode, c.redact(string(raw)))
	}
	if env.Success != 1 {
		var e struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(env.Data, &e)
		return &APIError{Code: e.Code, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return c.redactErr(fmt.Errorf("decode bitbank payload: %w", err))
	}
	return nil
}
