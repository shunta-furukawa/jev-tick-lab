package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
)

var start = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// writeLog lays down a tick log where the price at second i is 100+i, so a
// horizon price is its own check: +10s must be exactly 10 higher.
func writeLog(t *testing.T, dir string, seconds int, skip func(int) bool) string {
	t.Helper()
	path := filepath.Join(dir, "ticks-2026-09-17.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := newWriter(f)
	for i := 0; i < seconds; i++ {
		if skip != nil && skip(i) {
			continue
		}
		at := start.Add(time.Duration(i) * time.Second)
		if err := w.write(obs.Record{
			TickID:   at.Format(time.RFC3339Nano),
			At:       at,
			Pair:     "xrp_jpy",
			Snapshot: marketstate.Snapshot{At: at, Last: 100 + float64(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAll(t *testing.T, path string) []obs.Record {
	t.Helper()
	var out []obs.Record
	if err := obs.Scan(path, func(r obs.Record) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFillJoinsEachHorizon(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := writeLog(t, dir, 400, nil)
	out := filepath.Join(dir, "filled.jsonl")

	st, err := fill(in, out, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.total != 400 {
		t.Fatalf("total = %d, want 400", st.total)
	}

	got := readAll(t, out)
	if len(got) != 400 {
		t.Fatalf("wrote %d records, want 400", len(got))
	}
	first := got[0]
	if first.PriceAfter10s != 110 || first.PriceAfter60s != 160 || first.PriceAfter300s != 400 {
		t.Errorf("horizons = %v/%v/%v, want 110/160/400", first.PriceAfter10s, first.PriceAfter60s, first.PriceAfter300s)
	}

	// The tail of the file has no future to look at, and must say so rather
	// than reaching for the closest available price.
	last := got[len(got)-1]
	if last.PriceAfter10s != 0 || last.PriceAfter60s != 0 || last.PriceAfter300s != 0 {
		t.Errorf("the last record was filled from nothing: %+v", last)
	}
	if st.filled[10] != 390 {
		t.Errorf("filled %d records at +10s, want 390", st.filled[10])
	}
}

// A disconnect leaves a hole. Reaching across it would invent an outcome, and
// an invented outcome biases the calibration curve silently.
func TestFillLeavesAGapUnfilledRatherThanReachingAcrossIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Drop everything from +5s to +40s: the +10s horizon of the early records
	// falls inside the hole.
	in := writeLog(t, dir, 120, func(i int) bool { return i >= 5 && i < 40 })
	out := filepath.Join(dir, "filled.jsonl")

	if _, err := fill(in, out, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, out)

	if got[0].PriceAfter10s != 0 {
		t.Errorf("+10s = %v, want 0: that price is inside a 35s gap", got[0].PriceAfter10s)
	}
	if got[0].PriceAfter60s != 160 {
		t.Errorf("+60s = %v, want 160: that one lands after the gap", got[0].PriceAfter60s)
	}
}

// Within the slack window the nearest later price is good enough; past it, the
// horizon is left empty.
func TestSlackBoundsHowFarAPriceMayBeTaken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := writeLog(t, dir, 120, func(i int) bool { return i >= 10 && i < 12 }) // a 2s hole at +10s
	out := filepath.Join(dir, "filled.jsonl")

	if _, err := fill(in, out, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, out)[0].PriceAfter10s; got != 112 {
		t.Errorf("+10s = %v, want 112: the next price is 2s past the horizon, inside the slack", got)
	}

	if _, err := fill(in, out, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, out)[0].PriceAfter10s; got != 0 {
		t.Errorf("+10s = %v, want 0: 2s is outside a 1s slack", got)
	}
}

func TestFillKeepsFailedCallsAndTheirLabels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ticks.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := newWriter(f)
	for i := 0; i < 80; i++ {
		at := start.Add(time.Duration(i) * time.Second)
		rec := obs.Record{TickID: at.Format(time.RFC3339Nano), At: at, Snapshot: marketstate.Snapshot{At: at, Last: 100 + float64(i)}}
		if i == 0 {
			rec.Error = "typesafe 529: overloaded"
		}
		if err := w.write(rec); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	f.Close()

	out := filepath.Join(dir, "filled.jsonl")
	st, err := fill(path, out, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.errors != 1 {
		t.Errorf("counted %d failed calls, want 1", st.errors)
	}

	got := readAll(t, out)
	if got[0].Error == "" {
		t.Error("the failed call lost its error label")
	}
	if got[0].PriceAfter60s != 160 {
		t.Errorf("a failed call still has a market snapshot and should be filled: %v", got[0].PriceAfter60s)
	}
}

func TestFillRejectsAnEmptyLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fill(path, filepath.Join(dir, "out.jsonl"), time.Second); err == nil {
		t.Fatal("an empty log should be an error, not a silent empty output")
	}
}

func TestDefaultOut(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"data/ticks-2026-09-17.jsonl": "data/ticks-2026-09-17-filled.jsonl",
		"ticks.jsonl":                 "ticks-filled.jsonl",
	}
	for in, want := range cases {
		if got := defaultOut(in); got != want {
			t.Errorf("defaultOut(%q) = %q, want %q", in, got, want)
		}
	}
}
