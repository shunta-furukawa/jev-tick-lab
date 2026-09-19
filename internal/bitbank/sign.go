// Package bitbank is the private REST client: the only code in this repository
// that can move money.
//
// Three rules hold here and nowhere else matters as much.
//
// It cannot withdraw. The withdrawal endpoints are not implemented — not
// guarded by a flag, not gated behind a config option, simply absent. A bug
// cannot call a function that does not exist. The API key should also be
// issued without 出金 permission, so the restriction holds on both sides.
//
// It cannot leak the credential. Every error goes through redact, the same
// discipline internal/jev uses, because errors end up in JSONL records that
// are shipped to GCS and loaded into BigQuery, where a leak is permanent.
//
// It signs exactly the bytes it sends. The signature covers the request body
// verbatim, so the body is marshalled once and both signed and transmitted
// from the same slice. Re-marshalling between signing and sending is the
// classic way to produce a signature that is correct for a body nobody sent.
package bitbank

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// signTimeWindow is bitbank's ACCESS-TIME-WINDOW scheme: the signature covers
// the request time, the validity window, and then either the full request path
// (GET) or the request body (POST).
//
// Preferred over the nonce scheme because a nonce has to strictly increase
// forever. A restart with a clock that went backwards, or two requests inside
// the same millisecond, both break it — and both happen. A time window is
// stateless and says what it means: this request is valid for N milliseconds.
func signTimeWindow(secret string, requestTimeMs, windowMs int64, payload string) string {
	return sign(secret, strconv.FormatInt(requestTimeMs, 10)+strconv.FormatInt(windowMs, 10)+payload)
}

// signNonce is the older ACCESS-NONCE scheme, kept because it is what most of
// the vendor's own samples use and it is the fallback if a clock skew problem
// ever makes the time window unusable.
func signNonce(secret string, nonce int64, payload string) string {
	return sign(secret, strconv.FormatInt(nonce, 10)+payload)
}

func sign(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// redact removes the credential from a string, the same discipline
// internal/jev uses and for the same reason: errors from a failed call are
// written into a JSONL record, which is shipped to GCS and loaded into
// BigQuery, where a leak is permanent.
//
// The secret is never sent over the wire at all — only a signature derived
// from it — so it should be impossible for one to come back. This is here
// because "impossible" has to survive every future edit of this package.
func (c *Client) redact(s string) string {
	for _, v := range []string{c.secret, c.key} {
		// A short value would match half the diagnostic text and destroy it.
		// Real bitbank credentials are far longer than this.
		if len(v) < 8 {
			continue
		}
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}

// redactErr is redact for an error on its way out of this package.
func (c *Client) redactErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", c.redact(err.Error()))
}
