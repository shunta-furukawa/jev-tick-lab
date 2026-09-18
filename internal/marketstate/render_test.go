package marketstate

import (
	"strings"
	"testing"
	"time"
)

func sampleSnapshot() Snapshot {
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
	got := Render(sampleSnapshot(), Position{})

	const want = `# Market: xrp_jpy
Time: 2026-09-17T12:00:04Z

## Price
Last: 202.2900
Return 60s: +0.000%
Return 300s: +0.000%
SMA20: 202.2700 (price above)
SMA60: 202.2700 (price above)
5m high: 202.2900 / 5m low: 202.2500
Realized volatility (1s stdev, 60s): 0.0 bps

## Order book
Best bid: 202.2400 / Best ask: 202.2600
Spread: 0.0200 (0.989 bps)
Depth (top 10 levels): bid 1000.00 / ask 3000.00
Depth imbalance (bid share): 25%
Book age: 4000 ms

## Trade flow
Prints in last 30s: 5
Taker buy share (30s): 100%
Seconds since last print: 0

## Last 60 one-second closes (oldest first)
202.2500, 202.2600, 202.2700, 202.2800, 202.2900
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
	pos := Position{Side: "long", Size: 500, EntryPrice: 200, OpenedAt: t0}
	got := Render(sampleSnapshot(), pos)

	for _, want := range []string{"LONG 500.0000 @ 200.0000", "Unrealized: +1.145%", "Held for: 4s"} {
		if !strings.Contains(got, want) {
			t.Errorf("state text is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderWarnsAboutEveryUnhealthyCondition(t *testing.T) {
	t.Parallel()
	s := sampleSnapshot()
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
	got := Render(sampleSnapshot(), Position{})
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
	s, pos := sampleSnapshot(), Position{Side: "short", Size: 1, EntryPrice: 210, OpenedAt: t0}
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
