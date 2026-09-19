package exec

// Fees are in basis points of notional. Negative is a rebate.
//
// These are configuration and never constants: bitbank's negative maker fee is
// a campaign (the 2026-02-02 schedule) and ends when they say it does. A
// simulator with the rebate welded into it would keep reporting a profitable
// maker strategy for months after the rebate stopped existing.
type Fees struct {
	MakerBps float64
	TakerBps float64
}

// AltJPYFees is the schedule for every JPY pair except BTC/JPY.
//
// The round trip is what matters: all-taker is 24bps out and back, all-maker
// is a 4bps rebate. That 28bps gap is larger than most of the moves this
// experiment is trying to predict, which is the whole reason both paths are
// simulated side by side.
func AltJPYFees() Fees { return Fees{MakerBps: -2, TakerBps: 12} }

// BTCJPYFees is the BTC/JPY schedule: no rebate, and a cheaper taker.
func BTCJPYFees() Fees { return Fees{MakerBps: 0, TakerBps: 10} }

// FeesFor picks the schedule by pair. Unknown pairs get the alt schedule,
// which is the more expensive taker of the two — guessing in the direction
// that flatters the strategy is how a simulator becomes useless.
func FeesFor(pair string) Fees {
	if pair == "btc_jpy" {
		return BTCJPYFees()
	}
	return AltJPYFees()
}

// cost returns the fee paid on a notional, positive for a charge.
func (f Fees) cost(notional float64, maker bool) float64 {
	bps := f.TakerBps
	if maker {
		bps = f.MakerBps
	}
	return notional * bps / 10000
}
