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
	fmt.Fprintf(&b, "Spread: %.1f bps\n", s.SpreadBps)
	fmt.Fprintf(&b, "Depth (top %d levels): bid %.2f / ask %.2f\n", depthLevels, s.BidDepth, s.AskDepth)
	fmt.Fprintf(&b, "Depth imbalance (bid share): %.0f%%\n", s.DepthImbalance*100)
	fmt.Fprintf(&b, "Taker buy share (30s): %.0f%%\n\n", s.BuyRatio30s*100)

	fmt.Fprintf(&b, "## Last 60 one-second closes (oldest first)\n")
	fmt.Fprintf(&b, "%s\n\n", formatCloses(tail(closesOf(s.Bars), 60)))

	fmt.Fprintf(&b, "## Position\n")
	if pos.Size == 0 {
		fmt.Fprintf(&b, "Flat (no position).\n")
	} else {
		fmt.Fprintf(&b, "%s %.4f @ %.4f\n", strings.ToUpper(pos.Side), pos.Size, pos.EntryPrice)
		fmt.Fprintf(&b, "Unrealized: %+.3f%%\n", pos.UnrealizedPct(s.Last)*100)
		fmt.Fprintf(&b, "Held for: %.0fs\n", s.At.Sub(pos.OpenedAt).Seconds())
	}

	if s.Stale {
		b.WriteString("\n## Warning\nNo trades have printed recently; this data may be stale.\n")
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

func formatCloses(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.4f", x)
	}
	return strings.Join(parts, ", ")
}
