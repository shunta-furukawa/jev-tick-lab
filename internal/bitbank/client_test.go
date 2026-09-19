package bitbank

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fake(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := New("key-abcdefghijklmnop", "secret-abcdefghijklmnop")
	c.Endpoint = srv.URL
	c.now = func() time.Time { return time.UnixMilli(1721121776490) }
	return c, srv
}

func TestTheSignatureCoversTheExactBytesSent(t *testing.T) {
	t.Parallel()
	// The single most important property in this file. Marshalling the body
	// twice — once to sign, once to send — produces a signature that is valid
	// for a request nobody made, and the exchange rejects it with an opaque
	// authentication error.
	var gotBody []byte
	var gotSig string
	c, srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("ACCESS-SIGNATURE")
		w.Write([]byte(`{"success":1,"data":{"order_id":1,"status":"UNFILLED"}}`))
	})
	defer srv.Close()

	if _, err := c.PlaceOrder(context.Background(), NewOrder{
		Pair: "xrp_jpy", Amount: "13.4567", Price: "223.040", Side: "buy", Type: "limit", PostOnly: true,
	}); err != nil {
		t.Fatal(err)
	}

	want := signTimeWindow("secret-abcdefghijklmnop", 1721121776490,
		c.TimeWindow.Milliseconds(), string(gotBody))
	if gotSig != want {
		t.Errorf("signature does not cover the transmitted body:\n sent %s\n sig  %s\n want %s",
			gotBody, gotSig, want)
	}
}

func TestPostOnlyIsSentSoAMakerOrderCannotCross(t *testing.T) {
	t.Parallel()
	// Without post_only a limit order priced through the book crosses and pays
	// the taker fee, silently turning the -2bps path into the +12bps one.
	var body map[string]any
	c, srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"success":1,"data":{"order_id":1,"status":"UNFILLED"}}`))
	})
	defer srv.Close()

	c.PlaceOrder(context.Background(), NewOrder{
		Pair: "xrp_jpy", Amount: "1", Price: "223.040", Side: "buy", Type: "limit", PostOnly: true,
	})
	if body["post_only"] != true {
		t.Errorf("post_only not sent: %v", body)
	}
}

func TestAnErrorCodeIsReturnedRatherThanASuccess(t *testing.T) {
	t.Parallel()
	c, srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":0,"data":{"code":60001}}`))
	})
	defer srv.Close()

	_, err := c.PlaceOrder(context.Background(), NewOrder{Pair: "xrp_jpy", Amount: "1", Side: "buy", Type: "market"})
	if err == nil {
		t.Fatal("a success:0 envelope was treated as success")
	}
	ae, ok := err.(*APIError)
	if !ok || ae.Code != 60001 {
		t.Fatalf("err = %v, want APIError 60001", err)
	}
	// The message has to say what to do about it, not just echo a number.
	if ae.Error() == "" || !contains(ae.Error(), "balance") {
		t.Errorf("unhelpful error: %s", ae)
	}
}

func TestOrderRejectionsAreNeverRetryable(t *testing.T) {
	t.Parallel()
	// Retrying a rejected order is how one signal becomes three positions.
	// Only transport-level conditions, where the exchange plainly did not act,
	// may be retried.
	for _, code := range []int{40020, 50061, 60001, 70020, 30101} {
		e := &APIError{Code: code, Status: 200}
		if e.Retryable() {
			t.Errorf("code %d reported retryable; it is a decision, not a blip", code)
		}
	}
	for _, code := range []int{10001, 20001} {
		if !(&APIError{Code: code, Status: 200}).Retryable() {
			t.Errorf("code %d should be retryable", code)
		}
	}
}

func TestAnUnsupportedOrderTypeIsRefusedBeforeItIsSent(t *testing.T) {
	t.Parallel()
	// This experiment places limit and market orders. A stop order sent by
	// accident is a resting instruction nobody is tracking.
	var called bool
	c, srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"success":1,"data":{}}`))
	})
	defer srv.Close()

	if _, err := c.PlaceOrder(context.Background(), NewOrder{
		Pair: "xrp_jpy", Amount: "1", Side: "buy", Type: "stop_loss",
	}); err == nil {
		t.Fatal("a stop order was accepted")
	}
	if called {
		t.Error("the unsupported order reached the exchange")
	}
}

func TestThereIsNoWayToWithdraw(t *testing.T) {
	t.Parallel()
	// Not a runtime check — a statement about the type. If a withdrawal method
	// is ever added, this test is the thing that should have to be deleted
	// first, deliberately, by someone who read the comment.
	var c any = New("k", "s")
	type withdrawer interface {
		Withdraw(context.Context, string, string) error
	}
	if _, ok := c.(withdrawer); ok {
		t.Fatal("bitbank.Client has grown a Withdraw method; nothing in this experiment moves money off the exchange")
	}
}

func TestAStringifiedClientCannotPrintTheSecret(t *testing.T) {
	t.Parallel()
	c := New("key-abcdefghijklmnop", "secret-abcdefghijklmnop")
	s := c.String()
	for _, leak := range []string{"key-abcdefghijklmnop", "secret-abcdefghijklmnop"} {
		if contains(s, leak) {
			t.Errorf("String() printed the credential: %s", s)
		}
	}
}

func TestOrderDoneReflectsWhetherItCanStillFill(t *testing.T) {
	t.Parallel()
	for status, done := range map[string]bool{
		StatusUnfilled: false, StatusPartiallyFilled: false, StatusInactive: false,
		StatusFullyFilled: true, StatusCanceledUnfilled: true, StatusCanceledPartial: true,
	} {
		if got := (Order{Status: status}).Done(); got != done {
			t.Errorf("%s: Done() = %v, want %v", status, got, done)
		}
	}
}
