// Package stream implements a minimal bitbank public stream client.
//
// bitbank's public stream speaks Socket.IO v4 over a raw WebSocket. There is no
// need for a full Socket.IO library: the handshake and framing we need are a few
// string prefixes.
//
//	"0{...}"  engine.io OPEN     (server -> client, carries pingInterval)
//	"40"      socket.io CONNECT  (client -> server, and echoed back)
//	"2"       engine.io PING     (server -> client)
//	"3"       engine.io PONG     (client -> server, our reply)
//	"42[...]" socket.io EVENT    (both directions)
//
// Subscribing is an EVENT: 42["join-room","ticker_xrp_jpy"].
// Payloads arrive as:      42["message",{"room_name":"...","message":{"data":{...}}}]
//
// VERIFIED 2026-09-17 against a live connection to wss://stream.bitbank.cc —
// handshake, room names and envelope shape are as written above. The captured
// frames are in docs/stream-verification.md; re-run `go run ./cmd/dump` to check
// again after any exchange-side change.
package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultEndpoint is bitbank's public stream. Client.Endpoint overrides it,
// which is how the tests point the client at a local fake.
const DefaultEndpoint = "wss://stream.bitbank.cc/socket.io/?EIO=4&transport=websocket"

// Fallbacks used only until the server's OPEN packet tells us the real values.
const (
	defaultPingInterval = 25 * time.Second
	defaultPingTimeout  = 20 * time.Second
)

// Event is one decoded payload from a subscribed room.
type Event struct {
	Room     string
	Data     json.RawMessage
	Received time.Time
}

type envelope struct {
	RoomName string `json:"room_name"`
	Message  struct {
		Data json.RawMessage `json:"data"`
	} `json:"message"`
}

type openPacket struct {
	PingInterval int `json:"pingInterval"` // milliseconds
	PingTimeout  int `json:"pingTimeout"`
}

// Client holds the rooms to subscribe to and emits decoded events.
type Client struct {
	Rooms    []string
	Log      *slog.Logger
	Events   chan Event
	Endpoint string
}

func New(log *slog.Logger, rooms ...string) *Client {
	return &Client{
		Rooms:    rooms,
		Log:      log,
		Events:   make(chan Event, 1024),
		Endpoint: DefaultEndpoint,
	}
}

// Run connects and reconnects with backoff until ctx is cancelled.
// It never returns an error for transient failures — a trading process that
// exits because the network blipped is worse than one that retries.
func (c *Client) Run(ctx context.Context) {
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		// A session that stayed up is evidence the endpoint is healthy; only
		// repeated fast failures should slow us down.
		if time.Since(started) > time.Minute {
			backoff = minBackoff
		}
		c.Log.Warn("stream session ended, reconnecting", "err", err, "backoff", backoff, "uptime", time.Since(started).Round(time.Second).String())

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// Honour HTTPS_PROXY/NO_PROXY so the same binary works on a VM with
		// direct egress and behind an egress proxy.
		Proxy: http.ProxyFromEnvironment,
	}
	target := c.Endpoint
	if target == "" {
		target = DefaultEndpoint
	}
	conn, _, err := dialer.DialContext(ctx, target, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Kill the connection when the caller cancels, so the blocking read returns.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	connected := false
	idleLimit := defaultPingInterval + defaultPingTimeout
	_ = conn.SetReadDeadline(time.Now().Add(idleLimit))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		// Silence past one ping cycle means the far end is gone even if the
		// socket never reported it; the deadline turns that hang into a
		// reconnect.
		_ = conn.SetReadDeadline(time.Now().Add(idleLimit))
		msg := string(raw)

		switch {
		// engine.io OPEN — the server is ready for the socket.io handshake.
		case strings.HasPrefix(msg, "0{"):
			var open openPacket
			if json.Unmarshal(raw[1:], &open) == nil && open.PingInterval > 0 {
				idleLimit = time.Duration(open.PingInterval+open.PingTimeout) * time.Millisecond
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte("40")); err != nil {
				return fmt.Errorf("write connect: %w", err)
			}

		// socket.io CONNECT acknowledged — now we may join rooms.
		case msg == "40" || strings.HasPrefix(msg, "40{"):
			if connected {
				continue
			}
			connected = true
			for _, room := range c.Rooms {
				frame, _ := json.Marshal([]string{"join-room", room})
				if err := conn.WriteMessage(websocket.TextMessage, append([]byte("42"), frame...)); err != nil {
					return fmt.Errorf("join %s: %w", room, err)
				}
				c.Log.Info("joined room", "room", room)
			}

		// engine.io PING — reply immediately or the server drops us.
		case msg == "2":
			if err := conn.WriteMessage(websocket.TextMessage, []byte("3")); err != nil {
				return fmt.Errorf("pong: %w", err)
			}

		// socket.io EVENT — the actual market data.
		case strings.HasPrefix(msg, "42"):
			ev, ok := decodeEvent(raw)
			if !ok {
				continue
			}
			select {
			case c.Events <- ev:
			default:
				// Never block the reader. A full channel means the consumer is
				// behind; dropping the oldest work is the right call in a
				// latency-sensitive loop.
				c.Log.Warn("event channel full, dropping", "room", ev.Room)
			}

		// engine.io CLOSE and socket.io DISCONNECT / CONNECT_ERROR. Returning
		// hands control to Run, which reconnects.
		case msg == "1" || msg == "41" || strings.HasPrefix(msg, "44"):
			return fmt.Errorf("server closed the session: %s", msg)
		}
	}
}

// decodeEvent parses a socket.io EVENT frame into an Event. Frames that are not
// room messages (acks, unknown event names) are reported as not ok.
func decodeEvent(raw []byte) (Event, bool) {
	var frame []json.RawMessage
	if err := json.Unmarshal(raw[2:], &frame); err != nil || len(frame) < 2 {
		return Event{}, false
	}
	var name string
	if json.Unmarshal(frame[0], &name) != nil || name != "message" {
		return Event{}, false
	}
	var env envelope
	if json.Unmarshal(frame[1], &env) != nil || env.RoomName == "" {
		return Event{}, false
	}
	return Event{Room: env.RoomName, Data: env.Message.Data, Received: time.Now().UTC()}, true
}

// Room name helpers.
func TickerRoom(pair string) string           { return "ticker_" + pair }
func TransactionsRoom(pair string) string     { return "transactions_" + pair }
func DepthWholeRoom(pair string) string       { return "depth_whole_" + pair }
func DepthDiffRoom(pair string) string        { return "depth_diff_" + pair }
func CircuitBreakInfoRoom(pair string) string { return "circuit_break_info_" + pair }
