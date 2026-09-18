// Command fill joins each logged tick to the price some seconds later.
//
// Phase 3 needs an outcome for every record, and the outcome cannot be written
// at tick time: it does not exist yet. This is the forward-fill pass.
//
//	go run ./cmd/fill -in data/ticks-2026-09-17.jsonl
//	go run ./cmd/fill -in data/ticks-2026-09-17.jsonl -out /tmp/filled.jsonl
//
// The price series comes from the log itself — every record carries the market
// snapshot it was evaluated against, including the failed calls. Nothing is
// fetched from the exchange, so a filled file is reproducible from the raw one.
//
// A horizon whose price is missing is left empty rather than approximated. An
// invented outcome is worse than a missing one: it silently biases the
// calibration curve that is the point of the whole experiment.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

func main() {
	var (
		in    = flag.String("in", "", "input ticks-YYYY-MM-DD.jsonl (required)")
		out   = flag.String("out", "", "output file (default: <in> with -filled before the extension)")
		slack = flag.Duration("slack", 3*time.Second, "how far past a horizon a price may be taken from")
	)
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		flag.Usage()
		os.Exit(2)
	}
	target := *out
	if target == "" {
		target = defaultOut(*in)
	}

	stats, err := fill(*in, target, *slack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fill:", err)
		os.Exit(1)
	}

	fmt.Printf("read    %d records from %s\n", stats.total, *in)
	fmt.Printf("written %d records to   %s\n", stats.total, target)
	for _, h := range horizons {
		n := stats.filled[h.seconds]
		fmt.Printf("  +%-4ds filled %6d (%.1f%%)\n", h.seconds, n, pct(n, stats.total))
	}
	if stats.errors > 0 {
		fmt.Printf("  %d of those records are failed calls (kept: a labelled gap is data)\n", stats.errors)
	}
}

type horizon struct {
	seconds int
	set     func(*obs.Record, float64)
}

// The three horizons of the Record schema. Adding one means adding a field.
var horizons = []horizon{
	{10, func(r *obs.Record, v float64) { r.PriceAfter10s = v }},
	{60, func(r *obs.Record, v float64) { r.PriceAfter60s = v }},
	{300, func(r *obs.Record, v float64) { r.PriceAfter300s = v }},
}

type stats struct {
	total  int
	errors int
	filled map[int]int
}

// pricePoint is one observation of the market, taken from a logged snapshot.
type pricePoint struct {
	at    time.Time
	price float64
}

func fill(in, out string, slack time.Duration) (stats, error) {
	st := stats{filled: map[int]int{}}

	var (
		records []obs.Record
		series  []pricePoint
	)
	if err := obs.Scan(in, func(r obs.Record) error {
		records = append(records, r)
		if r.Snapshot.Last > 0 {
			series = append(series, pricePoint{at: r.At, price: r.Snapshot.Last})
		}
		if r.Error != "" {
			st.errors++
		}
		return nil
	}); err != nil {
		return st, err
	}
	st.total = len(records)
	if st.total == 0 {
		return st, fmt.Errorf("%s contains no records", in)
	}

	// Records are written in tick order, but a log can be concatenated from
	// several runs, so do not assume it.
	sort.Slice(series, func(i, j int) bool { return series[i].at.Before(series[j].at) })

	f, err := os.Create(out)
	if err != nil {
		return st, err
	}
	defer f.Close()

	w := newWriter(f)
	for i := range records {
		rec := records[i]
		for _, h := range horizons {
			if price, ok := priceAt(series, rec.At.Add(time.Duration(h.seconds)*time.Second), slack); ok {
				h.set(&rec, price)
				st.filled[h.seconds]++
			}
		}
		if err := w.write(rec); err != nil {
			return st, err
		}
	}
	return st, w.flush()
}

// priceAt returns the first observation at or after want, provided it is not
// more than slack past it. A gap in the log — a disconnect, a restart — leaves
// the horizon unfilled rather than reaching across it.
func priceAt(series []pricePoint, want time.Time, slack time.Duration) (float64, bool) {
	i := sort.Search(len(series), func(i int) bool { return !series[i].at.Before(want) })
	if i >= len(series) {
		return 0, false
	}
	if series[i].at.Sub(want) > slack {
		return 0, false
	}
	return series[i].price, true
}

func defaultOut(in string) string {
	dir, base := filepath.Split(in)
	ext := filepath.Ext(base)
	return filepath.Join(dir, strings.TrimSuffix(base, ext)+"-filled"+ext)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
