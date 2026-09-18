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
// NOTE: verify room names and payload fields against the official docs before
// trusting this in production: https://github.com/bitbankinc/bitbank-api-docs
package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const endpoint = "wss://stream.bitbank.cc/socket.io/?EIO=4&transport=websocket"

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

// Client holds the rooms to subscribe to and emits decoded events.
type Client struct {
	Rooms  []string
	Log    *slog.Logger
	Events chan Event
}

func New(log *slog.Logger, rooms ...string) *Client {
	return &Client{
		Rooms:  rooms,
		Log:    log,
		Events: make(chan Event, 1024),
	}
}

// Run connects and reconnects with backoff until ctx is cancelled.
// It never returns an error for transient failures — a trading process that
// exits because the network blipped is worse than one that retries.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		c.Log.Warn("stream session ended, reconnecting", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
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

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		msg := string(raw)

		switch {
		// engine.io OPEN — the server is ready for the socket.io handshake.
		case strings.HasPrefix(msg, "0{"):
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
			var frame []json.RawMessage
			if err := json.Unmarshal(raw[2:], &frame); err != nil || len(frame) < 2 {
				continue
			}
			var name string
			if json.Unmarshal(frame[0], &name) != nil || name != "message" {
				continue
			}
			var env envelope
			if json.Unmarshal(frame[1], &env) != nil {
				continue
			}
			select {
			case c.Events <- Event{Room: env.RoomName, Data: env.Message.Data, Received: time.Now()}:
			default:
				// Never block the reader. A full channel means the consumer is
				// behind; dropping the oldest work is the right call in a
				// latency-sensitive loop.
				c.Log.Warn("event channel full, dropping", "room", env.RoomName)
			}
		}
	}
}

// Room name helpers.
func TickerRoom(pair string) string       { return "ticker_" + pair }
func TransactionsRoom(pair string) string { return "transactions_" + pair }
func DepthWholeRoom(pair string) string   { return "depth_whole_" + pair }
func DepthDiffRoom(pair string) string    { return "depth_diff_" + pair }
