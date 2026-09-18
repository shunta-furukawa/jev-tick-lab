package marketstate

import "time"

// Position is the current holding. Size 0 means flat.
//
// This value is authoritative ONLY after it has been reconciled against the
// exchange. On startup the process must query bitbank for real balances and
// open orders rather than trusting anything it persisted locally.
type Position struct {
	Side       string // "long" | "short" | ""
	Size       float64
	EntryPrice float64
	OpenedAt   time.Time
}

func (p Position) IsFlat() bool { return p.Size == 0 }

// UnrealizedPct is the fractional P&L, before fees.
func (p Position) UnrealizedPct(last float64) float64 {
	if p.Size == 0 || p.EntryPrice == 0 {
		return 0
	}
	d := (last - p.EntryPrice) / p.EntryPrice
	if p.Side == "short" {
		return -d
	}
	return d
}
