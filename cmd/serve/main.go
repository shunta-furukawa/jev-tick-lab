// Command serve shows a tick log in a browser and keeps it current.
//
//	go run ./cmd/serve -dir ./data -tick 3s
//	open http://localhost:8080
//
// The same page cmd/report writes, except it rebuilds when the log grows and
// reloads itself. Point it at a running collector's directory and it is a live
// dashboard; point it at an archived day and it is a reader.
//
// It binds to localhost only. There is nothing to authenticate here and no
// reason for the run to be reachable from the network.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/report"
)

func main() {
	opt := report.DefaultOptions()
	var (
		dir  = flag.String("dir", "./data", "directory holding ticks-YYYY-MM-DD.jsonl")
		addr = flag.String("addr", "127.0.0.1:8080", "address to listen on; localhost by default")
	)
	flag.DurationVar(&opt.TickInterval, "tick", opt.TickInterval, "the cadence the run uses")
	flag.IntVar(&opt.HorizonSec, "horizon", opt.HorizonSec, "forward horizon for the reliability curve")
	flag.Float64Var(&opt.BandBps, "band-bps", opt.BandBps, "dead zone for the action outcome")
	flag.Parse()

	if _, err := os.Stat(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("jev-tick-lab — http://%s  (reading %s every few seconds)\n", *addr, *dir)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           report.Handler(*dir, opt),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
