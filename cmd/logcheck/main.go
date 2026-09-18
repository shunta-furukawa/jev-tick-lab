// Command logcheck answers one question: is the run still collecting a dataset
// worth analysing?
//
//	go run ./cmd/logcheck -dir ./data -window 1h
//	go run ./cmd/logcheck -dir /opt/jev-tick-lab/data -window 1h -json
//
// Exit status is the interface: 0 healthy, 1 degraded, 2 could not tell. A
// systemd timer runs it hourly, so a degraded window turns into a failed unit
// and a log line that Cloud Logging can alert on.
//
// It deliberately checks the DATA, not the process. systemd already restarts a
// dead bot. What it cannot see is a bot that is alive, writing a record every
// second, and writing records that are useless — evaluated against a book that
// never re-synced after a reconnect, or against a model version that moved
// halfway through the run.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/health"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

func main() {
	th := health.DefaultThresholds()
	var (
		dir    = flag.String("dir", "./data", "directory holding ticks-YYYY-MM-DD.jsonl")
		window = flag.Duration("window", time.Hour, "how far back to look")
		asJSON = flag.Bool("json", false, "emit one JSON object instead of a human summary")
		quiet  = flag.Bool("quiet", false, "print nothing when healthy (exit status still says so)")
	)
	flag.DurationVar(&th.TickInterval, "tick", th.TickInterval, "the cadence the bot was started with")
	flag.DurationVar(&th.MaxRecordAge, "max-record-age", th.MaxRecordAge, "fail if the newest record is older than this")
	flag.Float64Var(&th.MinRecordRate, "min-record-rate", th.MinRecordRate, "fail below this fraction of the expected record count")
	flag.Float64Var(&th.MaxErrorRate, "max-error-rate", th.MaxErrorRate, "fail above this share of failed model calls")
	flag.Float64Var(&th.MaxUnsyncedRate, "max-unsynced-rate", th.MaxUnsyncedRate, "fail above this share of ticks taken against an unseeded book")
	flag.Float64Var(&th.MaxStaleRate, "max-stale-rate", th.MaxStaleRate, "fail above this share of ticks taken against a stale feed")
	flag.DurationVar(&th.MaxGap, "max-gap", th.MaxGap, "fail if the series has a hole longer than this")
	flag.Parse()

	now := time.Now().UTC()
	records, err := load(*dir, now, *window)
	if err != nil {
		// Not being able to tell is its own status: a missing log directory is
		// not a healthy run, but it is also not a degraded one.
		fmt.Fprintln(os.Stderr, "logcheck:", err)
		os.Exit(2)
	}

	rep := health.Check(records, now, *window, th)

	if *asJSON {
		emitJSON(rep)
	} else if !rep.OK() || !*quiet {
		emitText(rep)
	}
	if !rep.OK() {
		os.Exit(1)
	}
}

// load reads the tick files that can contain the window. A window ending just
// after midnight UTC reaches into yesterday's file, so read both.
func load(dir string, now time.Time, window time.Duration) ([]obs.Record, error) {
	days := map[string]bool{
		now.Format("2006-01-02"):              true,
		now.Add(-window).Format("2006-01-02"): true,
	}

	var (
		records []obs.Record
		found   int
	)
	for day := range days {
		path := filepath.Join(dir, "ticks-"+day+".jsonl")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		found++
		if err := obs.Scan(path, func(r obs.Record) error {
			records = append(records, r)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if found == 0 {
		return nil, fmt.Errorf("no tick log in %s for %v", dir, keys(days))
	}
	return records, nil
}

func emitJSON(rep health.Report) {
	// Shaped like the bot's own slog output so Cloud Logging picks up the
	// severity and the fields without a parser of its own.
	payload := map[string]any{
		"time":    rep.At.Format(time.RFC3339Nano),
		"level":   rep.Severity(),
		"msg":     "logcheck",
		"healthy": rep.OK(),
		"report":  rep,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logcheck: marshal:", err)
		os.Exit(2)
	}
	fmt.Println(string(b))
}

func emitText(rep health.Report) {
	status := "HEALTHY"
	if !rep.OK() {
		status = "DEGRADED"
	}
	fmt.Printf("%s  window %s ending %s\n\n", status, rep.WindowS, rep.At.Format(time.RFC3339))

	fmt.Printf("  records        %d of an expected %d (%.1f%%)\n", rep.Records, rep.Expected, rep.RecordRate*100)
	if rep.Records == 0 {
		fmt.Printf("\n  %s\n", rep.Problems[0])
		return
	}
	fmt.Printf("  newest         %.0fs ago\n", rep.NewestAgeSec)
	fmt.Printf("  failed calls   %d (%.2f%%)\n", rep.Errors, rep.ErrorRate*100)
	fmt.Printf("  unseeded book  %d (%.2f%%)\n", rep.Unsynced, rep.UnsyncedRate*100)
	fmt.Printf("  stale feed     %d (%.2f%%)\n", rep.Stale, rep.StaleRate*100)
	fmt.Printf("  halted market  %d\n", rep.Halted)
	fmt.Printf("  holes          %d, longest %s, %.0fs missing\n", rep.Gaps, rep.LongestGapS, rep.MissingSec)
	fmt.Printf("  model          %v\n", rep.ModelVersions)
	fmt.Printf("  latency ms     p50 %.0f  p99 %.0f\n", rep.LatencyP50, rep.LatencyP99)

	if !rep.OK() {
		fmt.Println("\nproblems:")
		for _, p := range rep.Problems {
			fmt.Printf("  - %s\n", p)
		}
		fmt.Println("\nSee docs/operations.md for what each of these usually means.")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
