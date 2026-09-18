package marketstate

import (
	"strings"
	"testing"
	"time"
)

// matureSnapshot has more than five minutes of history, so every window the
// state text quotes is real. This is what production sends.
func matureSnapshot() Snapshot {
	b := NewBook("xrp_jpy")
	_ = b.ApplyDepthWhole(whole(1, t0.Add(-310*time.Second),
		[][2]string{{"202.240", "1000"}}, [][2]string{{"202.260", "3000"}}))
	for i := 0; i < 310; i++ {
		_ = b.ApplyTransactions(txs(Trade{
			At:     t0.Add(time.Duration(i-309) * time.Second),
			Side:   "buy",
			Price:  202.25 + float64(i%7)*0.01,
			Amount: 10,
		}))
	}
	return b.Snapshot(t0)
}

// youngSnapshot is a few seconds after connecting, which every run and every
// restart passes through.
func youngSnapshot() Snapshot {
	b := NewBook("xrp_jpy")
	_ = b.ApplyDepthWhole(whole(1, t0, [][2]string{{"202.240", "1000"}}, [][2]string{{"202.260", "3000"}}))
	for i := 0; i < 5; i++ {
		_ = b.ApplyTransactions(txs(Trade{
			At: t0.Add(time.Duration(i) * time.Second), Side: "buy", Price: 202.25 + float64(i)*0.01, Amount: 10,
		}))
	}
	return b.Snapshot(t0.Add(4 * time.Second))
}

// The state text is the single biggest lever on answer quality, so changes to
// it should be deliberate. This is a golden test: if it fails, look at the diff
// and decide whether the new text is better, then update the expectation.
func TestRenderGolden(t *testing.T) {
	t.Parallel()
	got := Render(matureSnapshot(), Position{})

	const want = `# Market: xrp_jpy
Time: 2026-09-17T12:00:00Z

## Price
Last: 202.2600
Return 60s: -0.015%
Return 300s: n/a (300s of history so far, needs 301s)
SMA20: 202.2805 (price below)
SMA60: 202.2800 (price below)
5m high: 202.3100 / 5m low: 202.2500
Realized volatility (1s stdev, 60s): 1.2 bps

## Order book
Best bid: 202.2400 / Best ask: 202.2600
Spread: 0.0200 (0.989 bps)
Depth (top 10 levels): bid 1000.00 / ask 3000.00
Depth imbalance (bid share): 25%
Book age: 310000 ms

## Trade flow
Prints in last 30s: 31
Taker buy share (30s): 100%
Seconds since last print: 0

## Last 60 one-second closes (oldest first)
202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600, 202.2700, 202.2800, 202.2900, 202.3000, 202.3100, 202.2500, 202.2600
Seconds with no print in that window: 0

## Position
Flat (no position).
`
	if got != want {
		t.Errorf("rendered state changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderIncludesPositionAndUnrealized(t *testing.T) {
	t.Parallel()
	pos := Position{Side: "long", Size: 500, EntryPrice: 200, OpenedAt: t0.Add(-90 * time.Second)}
	got := Render(matureSnapshot(), pos)

	for _, want := range []string{"LONG 500.0000 @ 200.0000", "Unrealized: +1.130%", "Held for: 90s"} {
		if !strings.Contains(got, want) {
			t.Errorf("state text is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderWarnsAboutEveryUnhealthyCondition(t *testing.T) {
	t.Parallel()
	s := matureSnapshot()
	s.Stale = true
	s.BookSynced = false
	s.CircuitBreak = "CIRCUIT_BREAK"

	got := Render(s, Position{})
	for _, want := range []string{"stale", "has not been seeded", "CIRCUIT_BREAK"} {
		if !strings.Contains(got, want) {
			t.Errorf("state text is missing a warning about %q:\n%s", want, got)
		}
	}
}

// A quiet market must not be presented as one-sided selling.
func TestRenderDistinguishesNoPrintsFromZeroBuying(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	_ = b.ApplyDepthWhole(whole(1, t0, [][2]string{{"202.240", "1000"}}, [][2]string{{"202.260", "3000"}}))
	_ = b.ApplyTicker(ticker(202.25, t0))

	got := Render(b.Snapshot(t0), Position{})
	if !strings.Contains(got, "Taker buy share (30s): n/a (no prints)") {
		t.Errorf("a window with no prints should say so:\n%s", got)
	}
	if !strings.Contains(got, "Seconds since last print: n/a (no prints seen)") {
		t.Errorf("a market that has never printed should say so:\n%s", got)
	}
}

// CLAUDE.md conventions: never emit a bare ratio. Every number in the state
// text carries a unit or an explicit label.
func TestRenderLabelsItsUnits(t *testing.T) {
	t.Parallel()
	got := Render(matureSnapshot(), Position{})
	for _, want := range []string{"bps", "%", "ms", "(oldest first)", "Z\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("state text is missing the %q labelling:\n%s", want, got)
		}
	}
}

// A logged snapshot must re-render to the same text months later, or the
// recorded run cannot be re-analysed.
func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	s, pos := matureSnapshot(), Position{Side: "short", Size: 1, EntryPrice: 210, OpenedAt: t0}
	first := Render(s, pos)
	for i := 0; i < 20; i++ {
		if got := Render(s, pos); got != first {
			t.Fatal("Render is not deterministic for a fixed snapshot")
		}
	}
}

func TestUnrealizedPct(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pos  Position
		last float64
		want float64
	}{
		{"flat is always zero", Position{}, 100, 0},
		{"long in profit", Position{Side: "long", Size: 1, EntryPrice: 100}, 110, 0.10},
		{"long in loss", Position{Side: "long", Size: 1, EntryPrice: 100}, 90, -0.10},
		{"short in profit", Position{Side: "short", Size: 1, EntryPrice: 100}, 90, 0.10},
		{"short in loss", Position{Side: "short", Size: 1, EntryPrice: 100}, 110, -0.10},
		{"no entry price cannot be valued", Position{Side: "long", Size: 1}, 110, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pos.UnrealizedPct(tc.last); got < tc.want-1e-9 || got > tc.want+1e-9 {
				t.Errorf("UnrealizedPct = %v, want %v", got, tc.want)
			}
		})
	}
}

// Preflight caught this against the live exchange: seconds after connecting, a
// perfectly healthy xrp_jpy book scored anomalous 0.54 against a 0.30 gate. The
// state text was telling the model the price had not moved in five minutes and
// volatility was exactly zero, which describes a halted venue rather than a
// young process.
func TestAYoungSeriesSaysSoRatherThanClaimingZero(t *testing.T) {
	t.Parallel()
	got := Render(youngSnapshot(), Position{})

	// Every window longer than the series must decline to answer.
	for _, label := range []string{"Return 60s", "Return 300s", "SMA20", "SMA60", "5m high", "Realized volatility"} {
		line := lineWith(got, label)
		if !strings.Contains(line, "n/a") {
			t.Errorf("%q reports a value it cannot have: %q", label, line)
		}
		if !strings.Contains(line, "history") {
			t.Errorf("%q does not say how much history is missing: %q", label, line)
		}
	}

	// The specific claims that read as a frozen market.
	for _, forbidden := range []string{"+0.000%", "-0.000%", "0.0 bps"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("a five-second-old series still asserts %q:\n%s", forbidden, got)
		}
	}
}

func TestHistorySeconds(t *testing.T) {
	t.Parallel()
	if got := youngSnapshot().HistorySeconds(); got != 5 {
		t.Errorf("young series = %ds, want 5", got)
	}
	if got := matureSnapshot().HistorySeconds(); got < 300 {
		t.Errorf("mature series = %ds, want at least 300", got)
	}
}

func lineWith(text, label string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, label) {
			return line
		}
	}
	return ""
}
