package marketstate

import (
	"fmt"
	"strings"
)

// Render turns a Snapshot into the text handed to Jev as `state`.
//
// Everything here is already computed. The model's job is judgement, not
// arithmetic. Keep this function boring and deterministic: it is the single
// biggest lever on answer quality, and it must be reproducible from a logged
// Snapshot so recorded runs can be re-rendered later.
//
// Every number carries its unit, because an unlabelled ratio is the easiest way
// to get a confidently wrong answer out of a model that has no way to ask.
func Render(s Snapshot, pos Position) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Market: %s\n", s.Pair)
	fmt.Fprintf(&b, "Time: %s\n\n", s.At.UTC().Format("2006-01-02T15:04:05Z"))

	fmt.Fprintf(&b, "## Price\n")
	fmt.Fprintf(&b, "Last: %.4f\n", s.Last)
	fmt.Fprintf(&b, "Return 60s: %+.3f%%\n", s.Ret60s*100)
	fmt.Fprintf(&b, "Return 300s: %+.3f%%\n", s.Ret300s*100)
	fmt.Fprintf(&b, "SMA20: %.4f (%s)\n", s.SMA20, relation(s.Last, s.SMA20))
	fmt.Fprintf(&b, "SMA60: %.4f (%s)\n", s.SMA60, relation(s.Last, s.SMA60))
	fmt.Fprintf(&b, "5m high: %.4f / 5m low: %.4f\n", s.High5m, s.Low5m)
	fmt.Fprintf(&b, "Realized volatility (1s stdev, 60s): %.1f bps\n\n", s.VolBps)

	fmt.Fprintf(&b, "## Order book\n")
	fmt.Fprintf(&b, "Best bid: %.4f / Best ask: %.4f\n", s.BestBid, s.BestAsk)
	// Both forms: on btc_jpy one tick is 0.0008 bps, which rounds away to
	// nothing, and on xrp_jpy the absolute figure is meaningless on its own.
	fmt.Fprintf(&b, "Spread: %.4f (%.3f bps)\n", s.BestAsk-s.BestBid, s.SpreadBps)
	fmt.Fprintf(&b, "Depth (top %d levels): bid %.2f / ask %.2f\n", depthLevels, s.BidDepth, s.AskDepth)
	fmt.Fprintf(&b, "Depth imbalance (bid share): %.0f%%\n", s.DepthImbalance*100)
	fmt.Fprintf(&b, "Book age: %.0f ms\n\n", s.BookAgeMs)

	fmt.Fprintf(&b, "## Trade flow\n")
	fmt.Fprintf(&b, "Prints in last 30s: %d\n", s.Trades30s)
	fmt.Fprintf(&b, "Taker buy share (30s): %s\n", percentOrNA(s.BuyRatio30s, s.Trades30s > 0))
	if s.LastTradeAt.IsZero() {
		// Distinct from "0 seconds ago", which reads as a market that just
		// traded rather than one that has not traded at all.
		fmt.Fprintf(&b, "Seconds since last print: n/a (no prints seen)\n\n")
	} else {
		fmt.Fprintf(&b, "Seconds since last print: %.0f\n\n", s.LastTradeAgo)
	}

	fmt.Fprintf(&b, "## Last 60 one-second closes (oldest first)\n")
	fmt.Fprintf(&b, "%s\n", formatCloses(tail(closesOf(s.Bars), 60)))
	fmt.Fprintf(&b, "Seconds with no print in that window: %d\n\n", untradedSeconds(s.Bars, 60))

	fmt.Fprintf(&b, "## Position\n")
	if pos.IsFlat() {
		fmt.Fprintf(&b, "Flat (no position).\n")
	} else {
		fmt.Fprintf(&b, "%s %.4f @ %.4f\n", strings.ToUpper(pos.Side), pos.Size, pos.EntryPrice)
		fmt.Fprintf(&b, "Unrealized: %+.3f%%\n", pos.UnrealizedPct(s.Last)*100)
		fmt.Fprintf(&b, "Held for: %.0fs\n", s.At.Sub(pos.OpenedAt).Seconds())
	}

	var warnings []string
	if s.Stale {
		warnings = append(warnings, "No market data has arrived recently; this snapshot may be stale.")
	}
	if !s.BookSynced {
		warnings = append(warnings, "The order book has not been seeded by a full snapshot; book figures are unavailable.")
	}
	if s.Halted() {
		warnings = append(warnings, fmt.Sprintf("The exchange reports circuit break mode %q; this market is not trading normally.", s.CircuitBreak))
	}
	if len(warnings) > 0 {
		b.WriteString("\n## Warning\n")
		for _, w := range warnings {
			b.WriteString(w + "\n")
		}
	}
	return b.String()
}

func relation(price, ref float64) string {
	switch {
	case ref == 0:
		return "n/a"
	case price > ref:
		return "price above"
	case price < ref:
		return "price below"
	default:
		return "at"
	}
}

// percentOrNA avoids reporting "0%" for a window with no trades at all, which
// reads as one-sided selling rather than as silence.
func percentOrNA(v float64, ok bool) string {
	if !ok {
		return "n/a (no prints)"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

func untradedSeconds(bars []Bar, n int) int {
	if len(bars) > n {
		bars = bars[len(bars)-n:]
	}
	var count int
	for _, b := range bars {
		if !b.Traded {
			count++
		}
	}
	return count
}

func formatCloses(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.4f", x)
	}
	return strings.Join(parts, ", ")
}
