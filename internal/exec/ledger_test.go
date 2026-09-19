package exec

import (
	"math"
	"testing"
	"time"
)

func entry(px, size float64, fee float64, at time.Time) Fill {
	return Fill{Intent: "open_long", Side: "buy", Price: px, Size: size,
		Notional: px * size, FeeJPY: fee, At: at}
}

func exit(px, size float64, fee float64, at time.Time) Fill {
	return Fill{Intent: "close", Side: "sell", Price: px, Size: size,
		Notional: px * size, FeeJPY: fee, At: at}
}

func TestACorrectTwentyBpsCallStillLosesMoneyTakingLiquidity(t *testing.T) {
	t.Parallel()
	// The single most important number in this project. An all-taker round
	// trip on a JPY alt is 24bps; predicting a 20bps move perfectly and
	// crossing the spread for it nets a loss. If this test ever passes with a
	// profit, the fee model has been broken and every result is fiction.
	f := AltJPYFees()
	l := NewLedger(Taker)

	const size = 48.0
	in, out := 207.50, 207.50*1.0020 // exactly +20bps

	l.Apply(entry(in, size, f.cost(in*size, false), now0))
	tr := l.Apply(exit(out, size, f.cost(out*size, false), now0.Add(time.Minute)))
	if tr == nil {
		t.Fatal("the exit did not close the position")
	}

	if tr.GrossJPY <= 0 {
		t.Fatalf("gross = %.4f, but the price rose", tr.GrossJPY)
	}
	if tr.NetJPY >= 0 {
		t.Errorf("net = %+.4f JPY (%.2f bps) — a 20bps winner must not pay after 24bps of fees",
			tr.NetJPY, tr.NetBps)
	}
	if tr.NetBps < -5 || tr.NetBps > -3 {
		t.Errorf("net = %.2f bps, want about -4 (20 gained, ~24 paid)", tr.NetBps)
	}
}

func TestTheSameCallPaysWhenBothLegsAreMaker(t *testing.T) {
	t.Parallel()
	// The other side of the same coin: the rebate turns the identical move
	// into a winner. This gap is why both paths are simulated rather than one.
	f := AltJPYFees()
	l := NewLedger(Maker)

	const size = 48.0
	in, out := 207.50, 207.50*1.0020

	l.Apply(entry(in, size, f.cost(in*size, true), now0))
	tr := l.Apply(exit(out, size, f.cost(out*size, true), now0.Add(time.Minute)))
	if tr.NetBps < 23 {
		t.Errorf("net = %.2f bps, want about +24 (20 gained, ~4 rebated)", tr.NetBps)
	}
	if l.FeesJPY() >= 0 {
		t.Errorf("fees = %.4f, want a net rebate", l.FeesJPY())
	}
}

func TestAShortRoundTripMakesMoneyWhenThePriceFalls(t *testing.T) {
	t.Parallel()
	l := NewLedger(Taker)
	l.Apply(Fill{Intent: "open_short", Side: "sell", Price: 207.50, Size: 10, Notional: 2075, At: now0})
	tr := l.Apply(Fill{Intent: "close", Side: "buy", Price: 205.00, Size: 10, Notional: 2050, At: now0.Add(time.Minute)})
	if tr == nil || tr.GrossJPY <= 0 {
		t.Fatalf("a short into a falling price should gain: %+v", tr)
	}
	if tr.Side != "short" {
		t.Errorf("side = %q", tr.Side)
	}
}

func TestAPartialExitLeavesTheRestOpenAndChargesFeesProRata(t *testing.T) {
	t.Parallel()
	f := AltJPYFees()
	l := NewLedger(Taker)
	l.Apply(entry(207.50, 100, f.cost(207.50*100, false), now0))

	tr := l.Apply(exit(208.00, 40, f.cost(208.00*40, false), now0.Add(time.Minute)))
	if tr == nil || tr.Size != 40 {
		t.Fatalf("partial exit = %+v", tr)
	}
	if got := l.Position().Size; math.Abs(got-60) > 1e-9 {
		t.Errorf("remaining size = %v, want 60", got)
	}
	// Only 40% of the entry fee belongs to this round trip.
	wantEntryShare := f.cost(207.50*100, false) * 0.4
	gotEntryShare := tr.FeeJPY - f.cost(208.00*40, false)
	if math.Abs(gotEntryShare-wantEntryShare) > 1e-9 {
		t.Errorf("entry fee charged = %.6f, want %.6f", gotEntryShare, wantEntryShare)
	}

	l.Apply(exit(208.00, 60, f.cost(208.00*60, false), now0.Add(2*time.Minute)))
	if !l.Position().IsFlat() {
		t.Error("the position should be flat after the rest is sold")
	}
	if _, total := l.Wins(); total != 2 {
		t.Errorf("round trips = %d, want 2", total)
	}
}

func TestAPartiallyFilledEntryAveragesRatherThanTakingTheLastSlice(t *testing.T) {
	t.Parallel()
	l := NewLedger(Maker)
	l.Apply(entry(207.00, 50, 0, now0))
	l.Apply(entry(209.00, 50, 0, now0.Add(time.Second)))

	if got := l.Position().EntryPrice; math.Abs(got-208.00) > 1e-9 {
		t.Errorf("entry price = %v, want the 208.00 average", got)
	}
	if got := l.Position().Size; got != 100 {
		t.Errorf("size = %v, want 100", got)
	}
	// And the open time is the first fill, not the last: hold time is measured
	// from when the position started existing.
	if !l.Position().OpenedAt.Equal(now0) {
		t.Errorf("opened at %v, want the first fill", l.Position().OpenedAt)
	}
}

func TestClosingWithNothingOpenChangesNothing(t *testing.T) {
	t.Parallel()
	l := NewLedger(Taker)
	if tr := l.Apply(exit(207.50, 10, 1, now0)); tr != nil {
		t.Errorf("closed a position that did not exist: %+v", tr)
	}
	if _, total := l.Wins(); total != 0 {
		t.Error("a phantom close produced a round trip")
	}
}

func TestMarkToMarketFollowsTheOpenPosition(t *testing.T) {
	t.Parallel()
	l := NewLedger(Taker)
	if l.MarkToMarket(207.50) != 0 {
		t.Error("a flat book has no unrealised P&L")
	}
	l.Apply(entry(207.50, 10, 2.49, now0))
	if got := l.MarkToMarket(208.50); got <= 0 {
		t.Errorf("unrealised = %.4f, want positive on a rise", got)
	}
	if got := l.MarkToMarket(206.50); got >= 0 {
		t.Errorf("unrealised = %.4f, want negative on a fall", got)
	}
}
