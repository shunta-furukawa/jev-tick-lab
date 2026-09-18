package health

import (
	"strings"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

type opt func(*obs.Record)

func failed(r *obs.Record)   { r.Error = "typesafe 529: overloaded"; r.ModelVersion = "" }
func unsynced(r *obs.Record) { r.Snapshot.BookSynced = false }
func stale(r *obs.Record)    { r.Snapshot.Stale = true }
func model(v string) opt     { return func(r *obs.Record) { r.ModelVersion = v } }

// series lays down one healthy record per second ending at now.
func series(n int, opts ...func(int, *obs.Record)) []obs.Record {
	out := make([]obs.Record, 0, n)
	for i := 0; i < n; i++ {
		at := now.Add(-time.Duration(n-1-i) * time.Second)
		rec := obs.Record{
			TickID:       at.Format(time.RFC3339Nano),
			At:           at,
			Pair:         "xrp_jpy",
			ModelVersion: "jev-1.13.0",
			LatencyMs:    400,
			Snapshot:     marketstate.Snapshot{At: at, Last: 202.25, BookSynced: true, CircuitBreak: "NONE"},
		}
		for _, o := range opts {
			o(i, &rec)
		}
		out = append(out, rec)
	}
	return out
}

func every(k int, o opt) func(int, *obs.Record) {
	return func(i int, r *obs.Record) {
		if i%k == 0 {
			o(r)
		}
	}
}

func hour() time.Duration { return time.Hour }

func TestAHealthyHourPasses(t *testing.T) {
	t.Parallel()
	rep := Check(series(3600), now, hour(), DefaultThresholds())
	if !rep.OK() {
		t.Fatalf("a full clean hour was reported unhealthy: %v", rep.Problems)
	}
	if rep.Records != 3600 || rep.Expected != 3600 {
		t.Errorf("records/expected = %d/%d, want 3600/3600", rep.Records, rep.Expected)
	}
	if rep.RecordRate != 1 {
		t.Errorf("record rate = %v, want 1", rep.RecordRate)
	}
	if rep.LatencyP50 != 400 {
		t.Errorf("p50 latency = %v, want 400", rep.LatencyP50)
	}
}

// The whole reason this runs on a timer: silence must be loud.
// What that silence means is TestAnEmptyLogSaysItMightJustBeWarmingUp.
func TestAnEmptyWindowIsAFailureNotACleanBillOfHealth(t *testing.T) {
	t.Parallel()
	rep := Check(nil, now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("no records at all was reported as healthy")
	}
	if len(rep.Problems) != 1 {
		t.Errorf("problems = %v, want exactly one", rep.Problems)
	}
}

func TestAStoppedBotIsCaughtByRecordAge(t *testing.T) {
	t.Parallel()
	// Wrote for ten minutes, then stopped half an hour ago.
	records := series(600)
	for i := range records {
		records[i].At = records[i].At.Add(-30 * time.Minute)
		records[i].Snapshot.At = records[i].At
	}

	rep := Check(records, now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("a bot that stopped half an hour ago was reported healthy")
	}
	if !containsMatch(rep.Problems, "newest record") {
		t.Errorf("problems = %v, want one about record age", rep.Problems)
	}
}

// Ticks going missing inside the span the log covers is a fault.
func TestLostTicksAreCaught(t *testing.T) {
	t.Parallel()
	// A full hour of coverage, but every sixth second dropped.
	full := series(3600)
	var thinned []obs.Record
	for i, rec := range full {
		if i%6 != 0 {
			thinned = append(thinned, rec)
		}
	}
	rep := Check(thinned, now, hour(), DefaultThresholds())

	if rep.OK() {
		t.Fatal("losing a sixth of the ticks was reported healthy")
	}
	if !containsMatch(rep.Problems, "ticks are being lost") {
		t.Errorf("problems = %v", rep.Problems)
	}
}

// A collection younger than the window is not a fault, and saying it is
// produces an alert that fires after every restart and then gets ignored.
func TestACollectionYoungerThanTheWindowIsNotAFault(t *testing.T) {
	t.Parallel()
	// Started thirty minutes ago, perfect since.
	rep := Check(series(1800), now, hour(), DefaultThresholds())

	if !rep.OK() {
		t.Fatalf("a young but complete collection was reported unhealthy: %v", rep.Problems)
	}
	if rep.RecordRate > 0.55 {
		t.Errorf("coverage = %.2f, want about 0.5 of the window", rep.RecordRate)
	}
	if rep.DensityRate < 0.99 {
		t.Errorf("density = %.3f, want ~1: every tick in the covered span landed", rep.DensityRate)
	}
	if got := time.Duration(rep.CoveredSec) * time.Second; got < 29*time.Minute || got > 31*time.Minute {
		t.Errorf("covered = %s, want about 30m", got)
	}
}

func TestAFewSkippedTicksAreNotAProblem(t *testing.T) {
	t.Parallel()
	// A slow call skips its tick by design; that must not read as a fault.
	records := series(3600)
	kept := records[:0]
	for i, rec := range records {
		if i%200 == 0 && i > 0 {
			continue
		}
		kept = append(kept, rec)
	}
	rep := Check(kept, now, hour(), DefaultThresholds())
	if !rep.OK() {
		t.Fatalf("occasional skipped ticks were reported unhealthy: %v", rep.Problems)
	}
	if rep.Gaps != 0 {
		t.Errorf("gaps = %d, want 0: a single skipped tick is within tolerance", rep.Gaps)
	}
}

func TestAHoleInTheSeriesIsReported(t *testing.T) {
	t.Parallel()
	records := series(3600)
	// Remove five minutes from the middle — a reconnect, or a restart.
	var kept []obs.Record
	for i, rec := range records {
		if i >= 1000 && i < 1300 {
			continue
		}
		kept = append(kept, rec)
	}

	th := DefaultThresholds()
	th.MinRecordRate = 0 // isolate the gap check from the rate check
	rep := Check(kept, now, hour(), th)

	if rep.Gaps != 1 {
		t.Errorf("gaps = %d, want 1", rep.Gaps)
	}
	if rep.LongestGap < 5*time.Minute {
		t.Errorf("longest gap = %v, want about 5m", rep.LongestGap)
	}
	if rep.MissingSec < 290 {
		t.Errorf("missing seconds = %v, want about 300", rep.MissingSec)
	}
	if !containsMatch(rep.Problems, "hole in the series") {
		t.Errorf("problems = %v", rep.Problems)
	}
}

func TestFailingCallsAreCaught(t *testing.T) {
	t.Parallel()
	rep := Check(series(3600, every(10, failed)), now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("a 10%% call failure rate was reported healthy")
	}
	if !containsMatch(rep.Problems, "calls failed") {
		t.Errorf("problems = %v", rep.Problems)
	}
	if rep.Errors != 360 {
		t.Errorf("errors = %d, want 360", rep.Errors)
	}
}

// The failure this whole package exists for: alive, writing, and useless.
func TestAnUnsyncedBookIsCaughtEvenThoughRecordsKeepArriving(t *testing.T) {
	t.Parallel()
	rep := Check(series(3600, every(5, unsynced)), now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("a fifth of the hour ran against an unseeded book and was reported healthy")
	}
	if !containsMatch(rep.Problems, "unseeded book") {
		t.Errorf("problems = %v", rep.Problems)
	}
}

func TestAStaleFeedIsCaught(t *testing.T) {
	t.Parallel()
	rep := Check(series(3600, every(4, stale)), now, hour(), DefaultThresholds())
	if !containsMatch(rep.Problems, "stale feed") {
		t.Errorf("problems = %v", rep.Problems)
	}
}

// Rule 5: a moved model invalidates every tuned threshold.
func TestAModelVersionChangeMidWindowIsAFailure(t *testing.T) {
	t.Parallel()
	records := series(3600, func(i int, r *obs.Record) {
		if i > 1800 {
			model("jev-1.14.0")(r)
		}
	})
	rep := Check(records, now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("a mid-window model change was reported healthy")
	}
	if !containsMatch(rep.Problems, "model version changed") {
		t.Errorf("problems = %v", rep.Problems)
	}
	if len(rep.ModelVersions) != 2 {
		t.Errorf("model versions = %v, want both", rep.ModelVersions)
	}
}

func TestRecordsOutsideTheWindowAreIgnored(t *testing.T) {
	t.Parallel()
	// A full clean hour, plus yesterday's records in the same file.
	old := series(3600)
	for i := range old {
		old[i].At = old[i].At.Add(-24 * time.Hour)
	}
	rep := Check(append(old, series(3600)...), now, hour(), DefaultThresholds())

	if !rep.OK() {
		t.Fatalf("problems = %v", rep.Problems)
	}
	if rep.Records != 3600 {
		t.Errorf("records in window = %d, want 3600", rep.Records)
	}
	if rep.Scanned != 7200 {
		t.Errorf("scanned = %d, want 7200", rep.Scanned)
	}
}

// A halted market is the exchange's doing, not a fault in the collection: the
// ticks are still data, and decide already stands aside.
func TestACircuitBreakIsCountedButNotAFailure(t *testing.T) {
	t.Parallel()
	records := series(3600, func(i int, r *obs.Record) { r.Snapshot.CircuitBreak = "CIRCUIT_BREAK" })
	rep := Check(records, now, hour(), DefaultThresholds())

	if !rep.OK() {
		t.Fatalf("a halted market was reported as a collection fault: %v", rep.Problems)
	}
	if rep.Halted != 3600 {
		t.Errorf("halted = %d, want 3600", rep.Halted)
	}
}

func TestEveryProblemNamesItsNumberAndItsLimit(t *testing.T) {
	t.Parallel()
	// Somebody reading this at 3am should not have to open the source.
	rep := Check(series(1000, every(3, failed)), now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("expected problems")
	}
	for _, p := range rep.Problems {
		if !strings.ContainsAny(p, "0123456789") {
			t.Errorf("problem %q quotes no numbers", p)
		}
	}
}

func containsMatch(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

// obs.NewLogger creates the tick file when the collector starts, before its
// first record. So "the file exists and is empty" is what a healthy run looks
// like during warmup, and also what a run that died on its first tick looks
// like. Reporting only "no records" leaves the reader to guess which.
func TestAnEmptyLogSaysItMightJustBeWarmingUp(t *testing.T) {
	t.Parallel()
	rep := Check(nil, now, hour(), DefaultThresholds())

	if rep.OK() {
		t.Fatal("an empty log is not healthy")
	}
	for _, want := range []string{"empty", "before its first record", "min-history"} {
		if !containsMatch(rep.Problems, want) {
			t.Errorf("problems = %v, want one mentioning %q", rep.Problems, want)
		}
	}
}

// The other zero-records case, which needs the opposite reaction.
func TestAStaleLogSaysTheCollectorStopped(t *testing.T) {
	t.Parallel()
	records := series(600)
	for i := range records {
		records[i].At = records[i].At.Add(-5 * time.Hour)
	}

	rep := Check(records, now, hour(), DefaultThresholds())
	if rep.OK() {
		t.Fatal("a log that stopped five hours ago is not healthy")
	}
	if !containsMatch(rep.Problems, "stopped") {
		t.Errorf("problems = %v, want one saying the collector stopped", rep.Problems)
	}
	if rep.Scanned != 600 {
		t.Errorf("scanned = %d, want 600 — the count is what distinguishes this from an empty log", rep.Scanned)
	}
	if got := now.Sub(rep.NewestOverall); got < 4*time.Hour {
		t.Errorf("newest overall is %s old, want about 5h", got)
	}
}
