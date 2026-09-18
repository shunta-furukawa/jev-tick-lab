// Command calib prints the calibration report for a filled tick log.
//
// This is the headline chart of the writeup, in text form: bucket the model's
// stated probability, and measure how often things it said with that
// probability actually happened.
//
//	go run ./cmd/fill  -in data/ticks-2026-09-17.jsonl
//	go run ./cmd/calib -in data/ticks-2026-09-17-filled.jsonl
//	go run ./cmd/calib -in data/ticks-2026-09-17-filled.jsonl -question momentum -outcome direction -horizon 60
//	go run ./cmd/calib -in data/ticks-2026-09-17-filled.jsonl -csv reliability.csv
//
// It reads only what cmd/fill wrote. Nothing here touches the exchange or the
// model, so a report can be regenerated from an archived log at any time.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/shunta-furukawa/jev-tick-lab/internal/calib"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

func main() {
	var (
		in       = flag.String("in", "", "filled ticks JSONL, comma-separated for several days (required)")
		question = flag.String("question", jev.QAction, "question id to calibrate")
		outcome  = flag.String("outcome", "", "action | direction (default: action for "+jev.QAction+", direction otherwise)")
		horizon  = flag.Int("horizon", 60, "forward horizon in seconds: 10, 60 or 300")
		bandBps  = flag.Float64("band-bps", 0, "dead zone around zero for the action outcome; 24 is an all-taker round trip on a JPY alt")
		bins     = flag.Int("bins", 10, "number of probability buckets")
		csvPath  = flag.String("csv", "", "also write the reliability table here, for plotting")
	)
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		flag.Usage()
		os.Exit(2)
	}
	mode := calib.Outcome(*outcome)
	if mode == "" {
		mode = calib.OutcomeDirection
		if *question == jev.QAction {
			mode = calib.OutcomeAction
		}
	}

	records, err := read(strings.Split(*in, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	rep, err := calib.Build(records, calib.Options{
		QuestionID: *question,
		Outcome:    mode,
		HorizonSec: *horizon,
		BandBps:    *bandBps,
		Bins:       *bins,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}

	printRun(records)
	printReport(rep)

	if *csvPath != "" {
		if err := writeCSV(*csvPath, rep); err != nil {
			fmt.Fprintln(os.Stderr, "csv:", err)
			os.Exit(1)
		}
		fmt.Printf("\nreliability table written to %s\n", *csvPath)
	}
}

func read(paths []string) ([]obs.Record, error) {
	var records []obs.Record
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := obs.Scan(p, func(r obs.Record) error {
			records = append(records, r)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// printRun is the context every number below has to be read against: which
// model answered, how often the call failed, and which brake stopped each tick.
func printRun(records []obs.Record) {
	var (
		models    = calib.Tally{}
		gates     = calib.Tally{}
		intents   = calib.Tally{}
		latencies []float64
		failures  int
		inTokens  int
		firstTick string
		lastTick  string
	)
	for _, r := range records {
		if firstTick == "" {
			firstTick = r.TickID
		}
		lastTick = r.TickID
		if r.Error != "" {
			failures++
			continue
		}
		models.Add(r.ModelVersion)
		gates.Add(gateName(string(r.Signal.Gate)))
		intents.Add(string(r.Signal.Intent))
		latencies = append(latencies, r.LatencyMs)
		inTokens += r.InputTokens
	}

	fmt.Println("== run ==")
	fmt.Printf("records            %d (%s .. %s)\n", len(records), firstTick, lastTick)
	fmt.Printf("failed calls       %d (%.2f%%)\n", failures, share(failures, len(records)))
	for _, m := range models.Sorted() {
		// A moved model invalidates tuned thresholds, so a report that spans
		// two model versions has to say so out loud.
		fmt.Printf("model              %s (%d records)\n", m, models[m])
	}
	if len(models) > 1 {
		fmt.Println("WARNING: this log spans more than one model version; the results are not comparable across them")
	}
	if len(latencies) > 0 {
		fmt.Printf("latency ms         p50 %.0f  p90 %.0f  p99 %.0f\n",
			calib.Percentile(latencies, 50), calib.Percentile(latencies, 90), calib.Percentile(latencies, 99))
		fmt.Printf("input tokens       %d total, %.0f per call\n", inTokens, float64(inTokens)/float64(len(latencies)))
	}

	// Shares are over the ticks that produced a signal, not over every line in
	// the file: a failed call has no gate, and counting it as a denominator
	// would quietly shrink every brake's share.
	answered := len(records) - failures

	fmt.Printf("\n== what stopped each tick (of %d answered) ==\n", answered)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, g := range gates.Sorted() {
		fmt.Fprintf(w, "%s\t%d\t%.1f%%\n", g, gates[g], share(gates[g], answered))
	}
	w.Flush()

	fmt.Printf("\n== intents (of %d answered) ==\n", answered)
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, i := range intents.Sorted() {
		fmt.Fprintf(w, "%s\t%d\t%.1f%%\n", i, intents[i], share(intents[i], answered))
	}
	w.Flush()
}

func printReport(rep calib.Report) {
	fmt.Printf("\n== calibration: %s, %s outcome, +%ds", rep.Options.QuestionID, rep.Options.Outcome, rep.Options.HorizonSec)
	if rep.Options.BandBps > 0 {
		fmt.Printf(", band %.0f bps", rep.Options.BandBps)
	}
	fmt.Println(" ==")

	fmt.Printf("usable observations %d of %d\n", rep.Usable, rep.Total)
	for _, reason := range calib.Tally(rep.Skipped).Sorted() {
		fmt.Printf("  skipped: %-28s %d\n", reason, rep.Skipped[reason])
	}
	if rep.Usable == 0 {
		fmt.Println("\nnothing to report. Has the log been through cmd/fill?")
		return
	}

	fmt.Printf("\nbase rate           %.3f\n", rep.BaseRate)
	fmt.Printf("Brier score         %.4f  (lower is better; %.4f is what always predicting the base rate scores)\n",
		rep.Brier, rep.BaseRate*(1-rep.BaseRate))
	fmt.Printf("expected calib err  %.4f  (mean gap between stated and realised)\n\n", rep.ECE)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "bucket\tn\tstated\trealised\tgap\t")
	for _, b := range rep.Bins {
		if b.N == 0 {
			fmt.Fprintf(w, "%.1f-%.1f\t0\t-\t-\t-\t\n", b.Lo, b.Hi)
			continue
		}
		fmt.Fprintf(w, "%.1f-%.1f\t%d\t%.3f\t%.3f\t%+.3f\t%s\n",
			b.Lo, b.Hi, b.N, b.MeanPredicted, b.Realised, b.Gap(), verdict(b))
	}
	w.Flush()

	fmt.Println("\nA positive gap is overconfidence: it happened less often than the model said.")
}

// verdict is a crude eyeball aid, not a statistical test. A bucket with a
// handful of observations says nothing either way.
func verdict(b calib.Bin) string {
	if b.N < 30 {
		return "(too few)"
	}
	switch gap := b.Gap(); {
	case gap > 0.15:
		return "overconfident"
	case gap < -0.15:
		return "underconfident"
	default:
		return ""
	}
}

func writeCSV(path string, rep calib.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"bucket_lo", "bucket_hi", "n", "stated", "realised", "gap"}); err != nil {
		return err
	}
	for _, b := range rep.Bins {
		row := []string{
			strconv.FormatFloat(b.Lo, 'f', 3, 64),
			strconv.FormatFloat(b.Hi, 'f', 3, 64),
			strconv.Itoa(b.N),
			strconv.FormatFloat(b.MeanPredicted, 'f', 6, 64),
			strconv.FormatFloat(b.Realised, 'f', 6, 64),
			strconv.FormatFloat(b.Gap(), 'f', 6, 64),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return w.Error()
}

func gateName(g string) string {
	if g == "" {
		return "(none — signal passed every gate)"
	}
	return g
}

func share(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
