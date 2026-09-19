package exec

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/bitbank"
	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/risk"
)

// Venue is the slice of bitbank the live executor uses. An interface so the
// order lifecycle is testable against a fake that can be made to behave
// badly — partial fills, timeouts, rejections — which is the only way to know
// what this does on the day it matters.
type Venue interface {
	PlaceOrder(ctx context.Context, o bitbank.NewOrder) (bitbank.Order, error)
	CancelOrder(ctx context.Context, pair string, id int64) (bitbank.Order, error)
	GetOrder(ctx context.Context, pair string, id int64) (bitbank.Order, error)
}

// LiveConfig is what the live executor is allowed to do.
type LiveConfig struct {
	Pair  string
	Rules bitbank.PairRules

	// Style decides whether entries cross the spread or rest at the touch.
	// Taker is the honest default for a first live run: it fills, so the run
	// produces data. Maker is cheaper and may produce almost no trades at all.
	Style Style

	// OrderTimeout cancels a resting order that has not filled.
	OrderTimeout time.Duration

	// PollInterval is how often a working order's status is read back.
	PollInterval time.Duration
}

func DefaultLiveConfig(pair string, rules bitbank.PairRules) LiveConfig {
	return LiveConfig{
		Pair: pair, Rules: rules, Style: Taker,
		OrderTimeout: 30 * time.Second,
		PollInterval: time.Second,
	}
}

// Live places real orders.
//
// It holds exactly one order at a time. Not a simplification — a deliberate
// limit: concurrent orders on one pair need a reconciliation model to keep
// them straight, and the failure mode of getting that wrong is two positions
// where there should be one. decide.Compose already forbids pyramiding, so
// there is nothing to gain by allowing it here.
type Live struct {
	mu  sync.Mutex
	cfg LiveConfig
	v   Venue

	pos     marketstate.Position
	working *bitbank.Order
	sentAt  time.Time

	// filled tracks how much of the working order has already been booked, so
	// a partial fill polled twice is not counted twice.
	booked float64

	ledger *Ledger
}

func NewLive(v Venue, cfg LiveConfig) *Live {
	return &Live{v: v, cfg: cfg, ledger: NewLedger(cfg.Style)}
}

// Adopt installs the position reconciliation found on the exchange.
func (l *Live) Adopt(pos marketstate.Position) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pos = pos
}

func (l *Live) Position() marketstate.Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pos
}

func (l *Live) Ledger() *Ledger { return l.ledger }

// Working reports whether an order is outstanding.
func (l *Live) Working() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.working != nil
}

// ErrBusy means an order is already outstanding. The tick loop skips rather
// than queues, exactly as it does for a slow model call: a second order placed
// on the strength of a stale decision is the accident this prevents.
var ErrBusy = fmt.Errorf("an order is already working")

// Submit turns a signal into a real order, subject to the verdict from the
// risk layer.
//
// verdict is passed in rather than consulted here so that the brakes stay in
// one place and stay pure. ExitOnly permits closes and refuses entries; Halt
// permits nothing.
//
// now is a parameter rather than a time.Now() call, the same discipline
// decide.Compose follows: the order timeout is measured from it, and a clock
// read inside here makes the timeout untestable — which is how an order that
// never cancels ships.
func (l *Live) Submit(ctx context.Context, now time.Time, sig decide.Signal, snap marketstate.Snapshot, verdict risk.Verdict, notionalJPY float64) (*bitbank.Order, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.working != nil {
		return nil, ErrBusy
	}
	if verdict == risk.Halt {
		return nil, nil
	}

	var side string
	var closing bool
	switch sig.Intent {
	case decide.IntentOpenLong:
		side = "buy"
	case decide.IntentOpenShort:
		// Spot has no borrow. See ErrShortOnSpot — the same rule as the
		// simulator, enforced again here because this one spends money.
		return nil, ErrShortOnSpot
	case decide.IntentClose:
		if l.pos.IsFlat() {
			return nil, nil
		}
		side, closing = "sell", true
	default:
		return nil, nil
	}

	if !closing && verdict != risk.Trade {
		return nil, nil // exit-only: no new exposure
	}
	if !l.cfg.Rules.TradingAllowed() {
		return nil, fmt.Errorf("bitbank is not accepting orders on %s", l.cfg.Pair)
	}

	size := l.pos.Size
	if !closing {
		ref := snap.Mid
		if ref <= 0 {
			ref = snap.Last
		}
		if ref <= 0 {
			return nil, fmt.Errorf("no price to size against")
		}
		size = notionalJPY / ref
	}
	if size < l.cfg.Rules.UnitAmount {
		// Below the venue minimum. For a close this means the remainder is
		// dust and the position is effectively flat; saying so is better than
		// sending an order that will be rejected every tick forever.
		if closing {
			l.pos = marketstate.Position{}
			return nil, nil
		}
		return nil, fmt.Errorf("size %.8f is below the %s minimum of %.8f",
			size, l.cfg.Pair, l.cfg.Rules.UnitAmount)
	}

	o := bitbank.NewOrder{
		Pair:   l.cfg.Pair,
		Amount: l.cfg.Rules.FormatAmount(size),
		Side:   side,
		Type:   "market",
	}
	if l.cfg.Style == Maker {
		px := snap.BestBid
		if side == "sell" {
			px = snap.BestAsk
		}
		if px <= 0 {
			return nil, fmt.Errorf("no touch to rest at")
		}
		o.Type = "limit"
		o.Price = l.cfg.Rules.FormatPrice(l.cfg.Rules.RoundPassive(px, side))
		// Maker-or-cancel. Without it a limit priced through the book crosses
		// and pays the taker fee, which turns the cheap path into the
		// expensive one without anything in the logs saying so.
		o.PostOnly = true
	}

	placed, err := l.v.PlaceOrder(ctx, o)
	if err != nil {
		return nil, err
	}
	l.working, l.sentAt, l.booked = &placed, now, 0
	return &placed, nil
}

// Poll reads the working order back and books whatever filled.
//
// Returns the fills that are new since the last call, plus the round trip if
// one closed. A partial fill polled twice must not be booked twice, which is
// what booked tracks.
//
// No lock is held across a network call. Holding one would stall the tick loop
// behind an exchange timeout, which is precisely when the loop most needs to
// keep running.
func (l *Live) Poll(ctx context.Context, now time.Time) ([]Execution, error) {
	l.mu.Lock()
	working := l.working
	sentAt := l.sentAt
	booked := l.booked
	l.mu.Unlock()
	if working == nil {
		return nil, nil
	}

	cur, err := l.v.GetOrder(ctx, l.cfg.Pair, working.OrderID)
	if err != nil {
		return nil, err
	}

	// Decide what to do before touching any shared state.
	newly := cur.Executed() - booked
	price := cur.AvgPrice()
	if newly > dust && price <= 0 {
		// Filled, but the exchange has not published an average yet. Fall back
		// to the limit price if there is one; for a market order there is
		// nothing honest to use, so wait for the next poll rather than
		// inventing a fill price.
		if working.Price == "" {
			newly = 0
		} else {
			price = bitbank.Num(working.Price)
		}
	}

	timedOut := !cur.Done() && l.cfg.OrderTimeout > 0 && now.Sub(sentAt) >= l.cfg.OrderTimeout
	if timedOut {
		if _, cancelErr := l.v.CancelOrder(ctx, l.cfg.Pair, cur.OrderID); cancelErr != nil {
			// Already finished is the common race. Anything else means the
			// order is still working and the caller must not be told it is
			// gone.
			if ae, ok := cancelErr.(*bitbank.APIError); !ok || (ae.Code != 50008 && ae.Code != 50009) {
				return nil, cancelErr
			}
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Execution
	if newly > dust {
		f := Fill{
			OrderID: fmt.Sprint(cur.OrderID), Style: l.cfg.Style, Side: cur.Side,
			At: now, Price: price, Size: newly, Notional: price * newly,
			WaitedMs: float64(now.Sub(sentAt).Milliseconds()),
		}
		f.Intent = decide.IntentOpenLong
		if cur.Side == "sell" {
			f.Intent = decide.IntentClose
		}
		// The fee comes from the venue's own schedule, for the style actually
		// used — not from a constant that could disagree with the invoice.
		bps := l.cfg.Rules.TakerFeeBps
		if l.cfg.Style == Maker {
			bps = l.cfg.Rules.MakerFeeBps
		}
		f.FeeJPY = f.Notional * bps / 10000

		l.booked = cur.Executed()
		l.applyLocked(f)
		out = append(out, Execution{Fill: f, Closed: l.ledger.Apply(f)})
	}
	if cur.Done() || timedOut {
		l.working = nil
	}
	return out, nil
}

// applyLocked folds a fill into the local position. Caller holds the lock.
func (l *Live) applyLocked(f Fill) {
	if f.Side == "buy" {
		total := l.pos.Size + f.Size
		if total <= 0 {
			return
		}
		l.pos.EntryPrice = (l.pos.EntryPrice*l.pos.Size + f.Price*f.Size) / total
		l.pos.Size, l.pos.Side = total, "long"
		if l.pos.OpenedAt.IsZero() {
			l.pos.OpenedAt = f.At
		}
		return
	}
	l.pos.Size -= f.Size
	if l.pos.Size <= dust {
		l.pos = marketstate.Position{}
	}
}

// CancelWorking withdraws whatever is outstanding. The shutdown path.
func (l *Live) CancelWorking(ctx context.Context) error {
	l.mu.Lock()
	working := l.working
	l.mu.Unlock()
	if working == nil {
		return nil
	}
	_, err := l.v.CancelOrder(ctx, l.cfg.Pair, working.OrderID)
	l.mu.Lock()
	l.working = nil
	l.mu.Unlock()
	if ae, ok := err.(*bitbank.APIError); ok && (ae.Code == 50008 || ae.Code == 50009) {
		return nil // already finished
	}
	return err
}
