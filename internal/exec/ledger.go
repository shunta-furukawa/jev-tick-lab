package exec

import (
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// RoundTrip is one completed position: in and out, net of fees.
type RoundTrip struct {
	Style Style
	Side  string // the side of the position: "long" | "short"

	OpenedAt time.Time
	ClosedAt time.Time
	EntryPx  float64
	ExitPx   float64
	Size     float64

	GrossJPY float64 // before fees
	FeeJPY   float64 // both legs
	NetJPY   float64
	NetBps   float64 // against the entry notional — the comparable number

	HeldSec float64
}

// Ledger turns fills into positions and closed round trips.
//
// It is deliberately separate from the Simulator: the simulator answers "would
// this order have filled, and at what price", the ledger answers "what did
// that do to the account". Keeping them apart is what makes it possible to run
// the same signal through a maker and a taker simulator and compare only the
// thing that differs.
//
// Pyramiding is not modelled because decide.Compose gates it (GatePyramid), so
// a second entry while holding is a bug upstream rather than a case to support
// here. An entry fill arriving while a position is open is folded into the
// average rather than silently dropped.
type Ledger struct {
	style Style
	pos   marketstate.Position
	// notional and fees accumulated on the open leg.
	openNotional float64
	openFees     float64

	trips  []RoundTrip
	netJPY float64
	fees   float64
}

func NewLedger(style Style) *Ledger { return &Ledger{style: style} }

func (l *Ledger) Position() marketstate.Position { return l.pos }
func (l *Ledger) RoundTrips() []RoundTrip        { return l.trips }
func (l *Ledger) NetJPY() float64                { return l.netJPY }
func (l *Ledger) FeesJPY() float64               { return l.fees }

// Wins is how many closed round trips made money net of fees. Reported beside
// the net, because a strategy can be right most of the time and still lose:
// that is the whole point of the 24bps round trip.
func (l *Ledger) Wins() (wins, total int) {
	for _, t := range l.trips {
		if t.NetJPY > 0 {
			wins++
		}
	}
	return wins, len(l.trips)
}

// Apply folds one fill into the ledger, returning a round trip if it closed one.
func (l *Ledger) Apply(f Fill) *RoundTrip {
	l.fees += f.FeeJPY

	opening := f.Intent != "close"
	if opening {
		side := "long"
		if f.Side == "sell" {
			side = "short"
		}
		// Average in, so a partially filled entry has a real entry price
		// rather than the price of whichever slice landed last.
		total := l.pos.Size + f.Size
		if total <= 0 {
			return nil
		}
		l.pos.EntryPrice = (l.pos.EntryPrice*l.pos.Size + f.Price*f.Size) / total
		l.pos.Size = total
		l.pos.Side = side
		if l.pos.OpenedAt.IsZero() {
			l.pos.OpenedAt = f.At
		}
		l.openNotional += f.Notional
		l.openFees += f.FeeJPY
		return nil
	}

	if l.pos.IsFlat() {
		return nil // nothing to close; the caller should not have submitted it
	}

	size := f.Size
	if size > l.pos.Size {
		size = l.pos.Size
	}
	share := size / l.pos.Size

	entryNotional := l.pos.EntryPrice * size
	exitNotional := f.Price * size

	gross := exitNotional - entryNotional
	if l.pos.Side == "short" {
		gross = -gross
	}
	// The entry leg's fee is charged pro rata to the size being closed.
	fee := l.openFees*share + f.FeeJPY

	t := RoundTrip{
		Style: l.style, Side: l.pos.Side,
		OpenedAt: l.pos.OpenedAt, ClosedAt: f.At,
		EntryPx: l.pos.EntryPrice, ExitPx: f.Price, Size: size,
		GrossJPY: gross, FeeJPY: fee, NetJPY: gross - fee,
		HeldSec: f.At.Sub(l.pos.OpenedAt).Seconds(),
	}
	if entryNotional > 0 {
		t.NetBps = t.NetJPY / entryNotional * 10000
	}

	l.openFees -= l.openFees * share
	l.openNotional -= entryNotional
	l.pos.Size -= size
	if l.pos.Size <= dust {
		l.pos = marketstate.Position{}
		l.openFees, l.openNotional = 0, 0
	}

	l.netJPY += t.NetJPY
	l.trips = append(l.trips, t)
	return &t
}

// MarkToMarket is the open position's unrealised P&L at a price, net of the
// entry fee already paid but not of the exit fee still to come.
func (l *Ledger) MarkToMarket(last float64) float64 {
	if l.pos.IsFlat() || last <= 0 {
		return 0
	}
	gross := (last - l.pos.EntryPrice) * l.pos.Size
	if l.pos.Side == "short" {
		gross = -gross
	}
	return gross - l.openFees
}
