package stream

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDecodeEvent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		frame    string
		wantOK   bool
		wantRoom string
		wantData string
	}{
		{
			name:     "a room message as bitbank actually sends it",
			frame:    `42["message",{"room_name":"depth_diff_xrp_jpy","message":{"pid":851203833,"data":{"a":[],"s":"34337088955"}}}]`,
			wantOK:   true,
			wantRoom: "depth_diff_xrp_jpy",
			wantData: `{"a":[],"s":"34337088955"}`,
		},
		{name: "an event that is not a room message", frame: `42["pong",{}]`},
		{name: "a message with no room", frame: `42["message",{"message":{"data":{}}}]`},
		{name: "a frame with only an event name", frame: `42["message"]`},
		{name: "malformed json", frame: `42[not json`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := decodeEvent([]byte(tc.frame))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if ev.Room != tc.wantRoom {
				t.Errorf("room = %q, want %q", ev.Room, tc.wantRoom)
			}
			if string(ev.Data) != tc.wantData {
				t.Errorf("data = %s, want %s", ev.Data, tc.wantData)
			}
			if ev.Received.IsZero() {
				t.Error("Received was not stamped")
			}
		})
	}
}

func TestRoomNames(t *testing.T) {
	t.Parallel()
	// These strings are the subscription contract; see docs/stream-verification.md.
	cases := map[string]string{
		TickerRoom("xrp_jpy"):           "ticker_xrp_jpy",
		TransactionsRoom("xrp_jpy"):     "transactions_xrp_jpy",
		DepthWholeRoom("xrp_jpy"):       "depth_whole_xrp_jpy",
		DepthDiffRoom("xrp_jpy"):        "depth_diff_xrp_jpy",
		CircuitBreakInfoRoom("xrp_jpy"): "circuit_break_info_xrp_jpy",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("room name = %q, want %q", got, want)
		}
	}
}

// fakeServer speaks just enough socket.io to exercise the client: OPEN,
// CONNECT, join acknowledgement, a PING, and one room message.
func fakeServer(t *testing.T, joins chan<- string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		write := func(msg string) bool {
			return conn.WriteMessage(websocket.TextMessage, []byte(msg)) == nil
		}
		if !write(`0{"sid":"abc","pingInterval":25000,"pingTimeout":20000}`) {
			return
		}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msg := string(raw)
			switch {
			case msg == "40":
				write(`40{"sid":"def"}`)
			case strings.HasPrefix(msg, `42["join-room"`):
				var frame []string
				if json.Unmarshal(raw[2:], &frame) == nil && len(frame) == 2 {
					select {
					case joins <- frame[1]:
					default:
					}
				}
				write(`2`) // a ping, to check the client answers it
				write(`42["message",{"room_name":"` + frame[1] + `","message":{"data":{"ok":true}}}]`)
			case msg == "3":
				// the client's pong; nothing to do
			}
		}
	}))
}

func TestSessionJoinsRoomsAndDeliversEvents(t *testing.T) {
	t.Parallel()
	joins := make(chan string, 4)
	srv := fakeServer(t, joins)
	defer srv.Close()

	c := New(discard(), "depth_whole_xrp_jpy")
	c.Endpoint = "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	select {
	case room := <-joins:
		if room != "depth_whole_xrp_jpy" {
			t.Fatalf("joined %q", room)
		}
	case <-ctx.Done():
		t.Fatal("client never joined a room")
	}

	select {
	case ev := <-c.Events:
		if ev.Room != "depth_whole_xrp_jpy" || string(ev.Data) != `{"ok":true}` {
			t.Fatalf("got event %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("client never delivered an event")
	}
}

// Dropping is the right behaviour when the consumer is behind: blocking the
// reader would back up the whole feed behind one slow tick.
func TestAFullChannelDropsRatherThanBlocks(t *testing.T) {
	t.Parallel()
	joins := make(chan string, 4)
	srv := fakeServer(t, joins)
	defer srv.Close()

	c := New(discard(), "ticker_xrp_jpy")
	c.Endpoint = "ws" + strings.TrimPrefix(srv.URL, "http")
	c.Events = make(chan Event) // nobody is reading

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	select {
	case <-joins:
	case <-ctx.Done():
		t.Fatal("client never joined")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return; the reader blocked on a full event channel")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	c := New(discard(), "ticker_xrp_jpy")
	c.Endpoint = "ws://127.0.0.1:1" // nothing is listening; Run must keep retrying

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run ignored context cancellation")
	}
}
