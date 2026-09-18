// Command dump connects to the bitbank public stream and prints raw frames.
//
// This is the verification tool for CLAUDE.md TODO 1: it deliberately does NOT
// use internal/stream, so what it prints is the wire, not our interpretation of
// it. Use it whenever a payload field is in doubt.
//
//	go run ./cmd/dump -pair xrp_jpy -rooms depth_whole,depth_diff -for 20s
//
// -truncate keeps a depth_whole snapshot (200 levels a side) readable; pass 0
// for the full frame.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const endpoint = "wss://stream.bitbank.cc/socket.io/?EIO=4&transport=websocket"

func main() {
	var (
		pair     = flag.String("pair", "xrp_jpy", "bitbank trading pair")
		rooms    = flag.String("rooms", "ticker,transactions,depth_whole,depth_diff,circuit_break_info", "comma-separated room prefixes")
		duration = flag.Duration("for", 15*time.Second, "how long to listen")
		truncate = flag.Int("truncate", 400, "truncate each frame to this many bytes (0 = no truncation)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	joined := false
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return // the deadline or a signal, not a failure
			}
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		msg := string(raw)
		fmt.Printf("%s  << %s\n", time.Now().UTC().Format("15:04:05.000"), clip(msg, *truncate))

		switch {
		case strings.HasPrefix(msg, "0{"):
			send(conn, "40")
		case msg == "40" || strings.HasPrefix(msg, "40{"):
			if joined {
				continue
			}
			joined = true
			for _, prefix := range strings.Split(*rooms, ",") {
				room := strings.TrimSpace(prefix) + "_" + *pair
				frame, _ := json.Marshal([]string{"join-room", room})
				send(conn, "42"+string(frame))
			}
		case msg == "2":
			send(conn, "3")
		}
	}
}

func send(conn *websocket.Conn, msg string) {
	fmt.Printf("%s  >> %s\n", time.Now().UTC().Format("15:04:05.000"), msg)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... (%d bytes total)", len(s))
}
