// Command report turns a tick log into a page you can look at.
//
//	go run ./cmd/report -in data/ticks-2026-09-18.jsonl -out report.html
//	go run ./cmd/report -in data/ticks-2026-09-18-filled.jsonl -horizon 60 -band-bps 24
//
// The output is one self-contained HTML file: no CDN, no fonts, no external
// scripts. It opens from a laptop, from a GCS bucket, and from an archive in a
// year's time, which is the same standard the JSONL itself is held to.
//
// Run it on a filled log and it also draws the reliability curve — the chart
// this whole experiment exists to produce. On a raw log it draws everything
// else and says the curve is missing.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
	"github.com/shunta-furukawa/jev-tick-lab/internal/report"
)

func main() {
	opt := report.DefaultOptions()
	var (
		in  = flag.String("in", "", "tick JSONL, comma-separated for several days (required)")
		out = flag.String("out", "", "output HTML (default: alongside the input)")
	)
	flag.DurationVar(&opt.TickInterval, "tick", opt.TickInterval, "the cadence the run used")
	flag.IntVar(&opt.HorizonSec, "horizon", opt.HorizonSec, "forward horizon for the reliability curve: 10, 60 or 300")
	flag.Float64Var(&opt.BandBps, "band-bps", opt.BandBps, "dead zone for the action outcome; 24 is an all-taker round trip on a JPY alt")
	flag.IntVar(&opt.Buckets, "buckets", opt.Buckets, "timeline resolution")
	flag.Float64Var(&opt.PricePerMTok, "price-per-mtok", opt.PricePerMTok, "USD per million input tokens")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		flag.Usage()
		os.Exit(2)
	}

	var records []obs.Record
	for _, path := range strings.Split(*in, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if err := obs.Scan(path, func(r obs.Record) error {
			records = append(records, r)
			return nil
		}); err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "no records in", *in)
		os.Exit(1)
	}

	rep := report.Build(records, opt)
	html, err := rep.HTML()
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}

	target := *out
	if target == "" {
		target = defaultOut(strings.Split(*in, ",")[0])
	}
	if err := os.WriteFile(target, []byte(html), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}

	fmt.Printf("%d records, %s → %s\n", rep.Records,
		rep.From.Format(time.RFC3339), rep.To.Format(time.RFC3339))
	fmt.Printf("wrote %s (%.0f KB)\n", target, float64(len(html))/1024)
	if !rep.HasOutcomes {
		fmt.Printf("\nNo forward prices at +%ds, so there is no reliability curve.\n"+
			"Run cmd/fill first if you want one:\n  go run ./cmd/fill -in %s\n",
			opt.HorizonSec, strings.Split(*in, ",")[0])
	}
}

func defaultOut(in string) string {
	dir, base := filepath.Split(in)
	return filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+".html")
}
