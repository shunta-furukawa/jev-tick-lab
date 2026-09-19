package exec

import (
	"fmt"
	"math"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// Style is how an order tries to get filled.
type Style string

const (
	// Taker crosses the spread: certain fill, immediate, pays the spread and
	// the taker fee.
	Taker Style = "taker"
	// Maker rests at the touch: cheaper, sometimes free, and may never fill —
	// and fills disproportionately when it should not.
	Maker Style = "maker"
)

// Config is everything the simulator is allowed to assume.
type Config struct {
	Pair string
	Fees Fees

	// NotionalJPY is the size of one entry, in yen.
	NotionalJPY float64

	// Latency is the round trip to bitbank: an order does not exist at the
	// exchange until it arrives. From the us-west1 VM this is ~110ms, from
	// Tokyo ~20ms — see the region owner decision in CLAUDE.md. Fills before
	// LiveAt are fills the order could not have got.
	Latency time.Duration

	// MakerTimeout cancels a resting order that has not filled. A passive
	// order left indefinitely is not a strategy, it is a lottery ticket on a
	// price that has moved on.
	MakerTimeout time.Duration

	// CrossAfterTimeout takes the spread when a maker entry times out. Off by
	// default: a strategy that always ends up crossing is a taker strategy
	// paying for the privilege of waiting first.
	CrossAfterTimeout bool

	// AllowShort is false for spot, where there is nothing to sell. See
	// Simulator.Submit.
	AllowShort bool
}

func DefaultConfig(pair string) Config {
	return Config{
		Pair:         pair,
		Fees:         FeesFor(pair),
		NotionalJPY:  10000,
		Latency:      150 * time.Millisecond,
		MakerTimeout: 30 * time.Second,
		AllowShort:   false, // spot only; see CLAUDE.md "Things deliberately not built"
	}
}

// Order is one working order.
type Order struct {
	ID     string
	Style  Style
	Side   string // "buy" | "sell"
	Size   float64
	Intent decide.Intent

	PlacedAt time.Time
	LiveAt   time.Time // PlacedAt + Latency

	// Maker only.
	LimitPrice float64
	QueueAhead float64 // size resting ahead of us at LimitPrice when we joined
}

// Fill is a completed execution.
type Fill struct {
	OrderID  string
	Style    Style
	Side     string
	Intent   decide.Intent
	At       time.Time
	Price    float64 // volume-weighted over the levels consumed
	Size     float64
	Notional float64
	FeeJPY   float64 // positive is paid, negative is a rebate
	SlipBps  float64 // against the mid at submission; 0 for a maker fill

	// WaitedMs is how long a maker order sat in the queue. It is the number
	// that decides whether a maker strategy is real: fills that only arrive
	// after twenty seconds arrive into a different market.
	WaitedMs float64
}

// Cancel is an order that gave up.
type Cancel struct {
	OrderID string
	Style   Style
	At      time.Time
	Reason  string
	// Unfilled is what was still resting. A maker order that never fills is
	// the most important output of this simulator and the easiest to lose.
	Unfilled float64
}

// Simulator fills orders against the real book and the real print stream.
//
// It is deliberately not a backtester. It is driven forward by live data: the
// caller feeds it the ladder as it changes and the prints as they arrive, and
// it answers what would have happened. Replaying it over stored prices would
// violate rule 1 in CLAUDE.md, and would also be wrong — the prints that fill
// a passive order are not in the tick log.
//
// The maker model is the part worth reading. A passive buy at the touch does
// not fill because the price "came down to it"; it fills because sellers hit
// the bid and consumed the size resting ahead of it. Driving fills from actual
// sell prints is what makes adverse selection fall out of the model rather
// than having to be bolted on: the order fills precisely in the moments the
// market is moving against it, and sits unfilled when it is right.
type Simulator struct {
	cfg Config

	working []*Order
	seq     int

	// midAtSubmit lets slippage be measured against the price the decision was
	// taken at, not the one the fill happened at.
	midAtSubmit map[string]float64
}

func New(cfg Config) *Simulator {
	if cfg.NotionalJPY <= 0 {
		cfg.NotionalJPY = 10000
	}
	if cfg.Fees == (Fees{}) {
		cfg.Fees = FeesFor(cfg.Pair)
	}
	return &Simulator{cfg: cfg, midAtSubmit: map[string]float64{}}
}

func (s *Simulator) Config() Config { return s.cfg }

// Working is how many orders are still live.
func (s *Simulator) Working() int { return len(s.working) }

// ErrShortOnSpot is returned when a short entry is asked for on a spot market.
//
// decide.Compose emits IntentOpenShort because the model is asked a symmetric
// question, but this experiment is spot only: there is no borrow, so there is
// nothing to sell. Silently filling one would put a position in the dataset
// that could never have existed. It is returned rather than logged and
// dropped so the caller has to record it.
var ErrShortOnSpot = fmt.Errorf("short entry on a spot market: nothing to sell")

// Submit places an order for a signal. The returned order is nil when the
// intent is not executable.
func (s *Simulator) Submit(sig decide.Signal, style Style, snap marketstate.Snapshot, pos marketstate.Position, now time.Time) (*Order, error) {
	var side string
	switch sig.Intent {
	case decide.IntentOpenLong:
		side = "buy"
	case decide.IntentOpenShort:
		if !s.cfg.AllowShort {
			return nil, ErrShortOnSpot
		}
		side = "sell"
	case decide.IntentClose:
		if pos.IsFlat() {
			return nil, nil
		}
		side = "sell"
		if pos.Side == "short" {
			side = "buy"
		}
	default:
		return nil, nil
	}

	if snap.BestBid <= 0 || snap.BestAsk <= 0 {
		return nil, fmt.Errorf("no book to trade against")
	}

	size := pos.Size
	if sig.Intent != decide.IntentClose {
		ref := snap.Mid
		if ref <= 0 {
			ref = snap.Last
		}
		if ref <= 0 {
			return nil, fmt.Errorf("no price to size against")
		}
		size = s.cfg.NotionalJPY / ref
	}
	if size <= 0 {
		return nil, nil
	}

	s.seq++
	o := &Order{
		ID:       fmt.Sprintf("o%d", s.seq),
		Style:    style,
		Side:     side,
		Size:     size,
		Intent:   sig.Intent,
		PlacedAt: now,
		LiveAt:   now.Add(s.cfg.Latency),
	}
	if style == Maker {
		// Join the touch on our own side. Joining the far side would be a
		// taker order wearing a maker's fee.
		if side == "buy" {
			o.LimitPrice = snap.BestBid
		} else {
			o.LimitPrice = snap.BestAsk
		}
	}
	s.midAtSubmit[o.ID] = snap.Mid
	s.working = append(s.working, o)
	return o, nil
}

// SeedQueue records how much size is resting ahead of a maker order, from the
// ladder as it stands when the order reaches the exchange.
//
// Joining a level means joining the back of it. Assuming the front — which is
// what a simulator that fills on "price touched the limit" implicitly does —
// is the single largest source of fictitious maker profit.
func (s *Simulator) SeedQueue(o *Order, bids, asks []marketstate.Level) {
	if o.Style != Maker {
		return
	}
	levels := bids
	if o.Side == "sell" {
		levels = asks
	}
	for _, l := range levels {
		if l.Price == o.LimitPrice {
			o.QueueAhead = l.Amount
			return
		}
	}
	// Our price is no longer on the book. Either it was consumed entirely in
	// the latency window, or the book moved away. Front of an empty queue.
	o.QueueAhead = 0
}

// OnPrints advances every resting maker order by the prints that have happened
// since the last call, and returns whatever filled.
//
// A buy at limit P is filled by SELL prints (a taker hitting the bid):
//   - a print below P means the book traded through our level, so everything
//     resting at P, including us, is gone;
//   - a print at P consumes the queue ahead of us first, and only the
//     remainder touches our order.
func (s *Simulator) OnPrints(prints []marketstate.Trade, now time.Time) []Fill {
	var fills []Fill
	for _, o := range s.working {
		if o.Style != Maker || now.Before(o.LiveAt) {
			continue
		}
		for _, p := range prints {
			if p.At.Before(o.LiveAt) {
				continue // it happened before our order existed
			}
			take := s.consume(o, p)
			if take <= 0 {
				continue
			}
			o.Size -= take
			fills = append(fills, s.fill(o, o.LimitPrice, take, p.At, true))
			if o.Size <= dust {
				break
			}
		}
	}
	s.reap()
	return fills
}

// consume is how much of a print reaches our order.
func (s *Simulator) consume(o *Order, p marketstate.Trade) float64 {
	through, at := false, false
	if o.Side == "buy" {
		// Sellers hitting the bid fill a resting buy.
		if p.Side != "sell" {
			return 0
		}
		through, at = p.Price < o.LimitPrice, p.Price == o.LimitPrice
	} else {
		if p.Side != "buy" {
			return 0
		}
		through, at = p.Price > o.LimitPrice, p.Price == o.LimitPrice
	}

	switch {
	case through:
		// The book cleared our level on the way past.
		o.QueueAhead = 0
		return o.Size
	case at:
		vol := p.Amount
		if o.QueueAhead > 0 {
			eaten := math.Min(vol, o.QueueAhead)
			o.QueueAhead -= eaten
			vol -= eaten
		}
		return math.Min(vol, o.Size)
	}
	return 0
}

// OnBook fills taker orders that have reached the exchange, and times out
// maker orders that have waited too long.
func (s *Simulator) OnBook(bids, asks []marketstate.Level, now time.Time) ([]Fill, []Cancel) {
	var fills []Fill
	var cancels []Cancel

	for _, o := range s.working {
		if now.Before(o.LiveAt) {
			continue
		}
		switch o.Style {
		case Taker:
			levels := asks // buying lifts the offer
			if o.Side == "sell" {
				levels = bids
			}
			price, filled := walk(levels, o.Size)
			if filled <= 0 {
				continue
			}
			o.Size -= filled
			fills = append(fills, s.fill(o, price, filled, now, false))

		case Maker:
			if s.cfg.MakerTimeout > 0 && now.Sub(o.LiveAt) >= s.cfg.MakerTimeout {
				if s.cfg.CrossAfterTimeout {
					levels := asks
					if o.Side == "sell" {
						levels = bids
					}
					if price, filled := walk(levels, o.Size); filled > 0 {
						o.Size -= filled
						fills = append(fills, s.fill(o, price, filled, now, false))
						continue
					}
				}
				cancels = append(cancels, Cancel{
					OrderID: o.ID, Style: o.Style, At: now,
					Reason: "maker timeout", Unfilled: o.Size,
				})
				o.Size = 0
			}
		}
	}
	s.reap()
	return fills, cancels
}

// CancelAll gives up on everything still working — the shutdown path.
func (s *Simulator) CancelAll(now time.Time, reason string) []Cancel {
	out := make([]Cancel, 0, len(s.working))
	for _, o := range s.working {
		out = append(out, Cancel{OrderID: o.ID, Style: o.Style, At: now, Reason: reason, Unfilled: o.Size})
		o.Size = 0
	}
	s.reap()
	return out
}

// dust is a size below which an order is finished. Floating-point subtraction
// leaves remainders that would otherwise keep an order working forever.
const dust = 1e-9

func (s *Simulator) reap() {
	kept := s.working[:0]
	for _, o := range s.working {
		if o.Size > dust {
			kept = append(kept, o)
			continue
		}
		delete(s.midAtSubmit, o.ID)
	}
	s.working = kept
}

func (s *Simulator) fill(o *Order, price, size float64, at time.Time, maker bool) Fill {
	notional := price * size
	f := Fill{
		OrderID: o.ID, Style: o.Style, Side: o.Side, Intent: o.Intent,
		At: at, Price: price, Size: size, Notional: notional,
		FeeJPY:   s.cfg.Fees.cost(notional, maker),
		WaitedMs: float64(at.Sub(o.LiveAt).Milliseconds()),
	}
	if mid := s.midAtSubmit[o.ID]; mid > 0 && !maker {
		slip := (price - mid) / mid * 10000
		if o.Side == "sell" {
			slip = -slip
		}
		f.SlipBps = slip
	}
	return f
}

// walk consumes size from a ladder and returns the volume-weighted price.
//
// It fills only what the visible book can cover. A simulator that assumes the
// rest arrives at the last price is inventing liquidity, and it invents most
// of it exactly when the book is thin — which is when it matters.
func walk(levels []marketstate.Level, size float64) (vwap, filled float64) {
	var cost float64
	for _, l := range levels {
		if filled >= size-dust {
			break
		}
		take := math.Min(l.Amount, size-filled)
		cost += take * l.Price
		filled += take
	}
	if filled <= 0 {
		return 0, 0
	}
	return cost / filled, filled
}
