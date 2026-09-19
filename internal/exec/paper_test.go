package exec

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

var now0 = time.Date(2026, 9, 19, 6, 0, 0, 0, time.UTC)

func book() (bids, asks []marketstate.Level) {
	bids = []marketstate.Level{{Price: 207.50, Amount: 100}, {Price: 207.49, Amount: 400}, {Price: 207.48, Amount: 900}}
	asks = []marketstate.Level{{Price: 207.52, Amount: 80}, {Price: 207.53, Amount: 300}, {Price: 207.54, Amount: 700}}
	return bids, asks
}

func snap() marketstate.Snapshot {
	return marketstate.Snapshot{
		At: now0, Pair: "xrp_jpy", Last: 207.51,
		BestBid: 207.50, BestAsk: 207.52, Mid: 207.51,
		BookSynced: true, CircuitBreak: "NONE",
	}
}

func openLong() decide.Signal {
	return decide.Signal{At: now0, Intent: decide.IntentOpenLong}
}

func sim(mut ...func(*Config)) *Simulator {
	c := DefaultConfig("xrp_jpy")
	c.Latency = 100 * time.Millisecond
	for _, m := range mut {
		m(&c)
	}
	return New(c)
}

func TestTakerWalksTheLadderInsteadOfFillingAtTheTouch(t *testing.T) {
	t.Parallel()
	// 10,000 JPY at ~207.51 is ~48 XRP, which the 80 at the touch covers. Size
	// it past the touch instead: a simulator that always fills at the best
	// offer reports a strategy that does not exist above a trivial size.
	s := sim(func(c *Config) { c.NotionalJPY = 207.52*80 + 207.53*100 })
	bids, asks := book()

	if _, err := s.Submit(openLong(), Taker, snap(), marketstate.Position{}, now0); err != nil {
		t.Fatal(err)
	}
	fills, _ := s.OnBook(bids, asks, now0.Add(200*time.Millisecond))
	if len(fills) != 1 {
		t.Fatalf("fills = %d, want 1", len(fills))
	}

	f := fills[0]
	if f.Price <= 207.52 {
		t.Errorf("filled at %.4f — the touch, not the walk", f.Price)
	}
	if f.Price >= 207.53 {
		t.Errorf("filled at %.4f — past the second level it only partly ate", f.Price)
	}
	if f.SlipBps <= 0 {
		t.Errorf("slippage = %.2f bps, want positive: buying above the mid costs money", f.SlipBps)
	}
	if f.FeeJPY <= 0 {
		t.Error("a taker fill on an alt pays a fee")
	}
}

func TestTakerFillsOnlyWhatTheBookCanCover(t *testing.T) {
	t.Parallel()
	// The visible ladder holds 1,080 units. Ask for more and the rest must not
	// be invented at the last price — thin books are exactly when a simulator
	// that extrapolates lies most.
	bids, asks := book()
	var visible float64
	for _, l := range asks {
		visible += l.Amount
	}
	s := sim(func(c *Config) { c.NotionalJPY = 207.51 * visible * 2 })
	if _, err := s.Submit(openLong(), Taker, snap(), marketstate.Position{}, now0); err != nil {
		t.Fatal(err)
	}

	fills, _ := s.OnBook(bids, asks, now0.Add(200*time.Millisecond))
	if len(fills) != 1 {
		t.Fatalf("fills = %d", len(fills))
	}
	if got := fills[0].Size; math.Abs(got-visible) > 1e-6 {
		t.Errorf("filled %.4f of a %.4f book", got, visible)
	}
	if s.Working() != 1 {
		t.Error("the unfilled remainder should still be working, not quietly gone")
	}
}

func TestAnOrderCannotFillBeforeItReachesTheExchange(t *testing.T) {
	t.Parallel()
	s := sim(func(c *Config) { c.Latency = 500 * time.Millisecond })
	bids, asks := book()
	if _, err := s.Submit(openLong(), Taker, snap(), marketstate.Position{}, now0); err != nil {
		t.Fatal(err)
	}

	if fills, _ := s.OnBook(bids, asks, now0.Add(100*time.Millisecond)); len(fills) != 0 {
		t.Fatal("filled inside the latency window — the order was still in flight")
	}
	if fills, _ := s.OnBook(bids, asks, now0.Add(600*time.Millisecond)); len(fills) != 1 {
		t.Fatal("did not fill after arriving")
	}
}

func TestAMakerOrderJoinsTheBackOfTheQueue(t *testing.T) {
	t.Parallel()
	// 100 units rest at 207.50 when we join. Prints totalling less than that
	// must not touch us: assuming the front of the queue is the single largest
	// source of maker profit that does not exist.
	s := sim()
	bids, asks := book()
	o, err := s.Submit(openLong(), Maker, snap(), marketstate.Position{}, now0)
	if err != nil {
		t.Fatal(err)
	}
	s.SeedQueue(o, bids, asks)
	if o.QueueAhead != 100 {
		t.Fatalf("queue ahead = %v, want 100", o.QueueAhead)
	}

	at := now0.Add(time.Second)
	if f := s.OnPrints([]marketstate.Trade{
		{ID: 1, At: at, Side: "sell", Price: 207.50, Amount: 60},
	}, at); len(f) != 0 {
		t.Fatal("filled with 40 units still ahead of us")
	}

	// The rest of the queue, then us.
	at = at.Add(time.Second)
	fills := s.OnPrints([]marketstate.Trade{
		{ID: 2, At: at, Side: "sell", Price: 207.50, Amount: 40 + 10},
	}, at)
	if len(fills) != 1 {
		t.Fatalf("fills = %d, want 1 once the queue cleared", len(fills))
	}
	if got := fills[0].Size; math.Abs(got-10) > 1e-9 {
		t.Errorf("filled %.4f, want the 10 units past the queue", got)
	}
	if fills[0].Price != 207.50 {
		t.Errorf("maker filled at %.4f, not its own limit", fills[0].Price)
	}
	if fills[0].FeeJPY >= 0 {
		t.Error("an alt maker fill earns the rebate; fee should be negative")
	}
}

func TestAMakerBuyIsFilledBySellersNotByTheQuotedPrice(t *testing.T) {
	t.Parallel()
	// This is the adverse-selection property. A buy resting at the bid fills
	// when sellers hit it — i.e. when the market is going down. Buy prints at
	// the offer, however many, must never fill it.
	s := sim()
	bids, asks := book()
	o, _ := s.Submit(openLong(), Maker, snap(), marketstate.Position{}, now0)
	s.SeedQueue(o, bids, asks)

	at := now0.Add(time.Second)
	buys := make([]marketstate.Trade, 0, 20)
	for i := 0; i < 20; i++ {
		buys = append(buys, marketstate.Trade{ID: int64(i + 1), At: at, Side: "buy", Price: 207.52, Amount: 500})
	}
	if f := s.OnPrints(buys, at); len(f) != 0 {
		t.Fatal("a rally filled a resting bid; the model has the sides backwards")
	}

	// One seller through our level clears it, and us with it.
	at = at.Add(time.Second)
	if f := s.OnPrints([]marketstate.Trade{
		{ID: 99, At: at, Side: "sell", Price: 207.49, Amount: 1000},
	}, at); len(f) != 1 {
		t.Fatal("a print through the limit must fill the whole order")
	}
}

func TestAPrintOlderThanTheOrderCannotFillIt(t *testing.T) {
	t.Parallel()
	s := sim(func(c *Config) { c.Latency = time.Second })
	bids, asks := book()
	o, _ := s.Submit(openLong(), Maker, snap(), marketstate.Position{}, now0)
	s.SeedQueue(o, bids, asks)

	at := now0.Add(2 * time.Second)
	stale := marketstate.Trade{ID: 1, At: now0.Add(500 * time.Millisecond), Side: "sell", Price: 207.40, Amount: 9999}
	if f := s.OnPrints([]marketstate.Trade{stale}, at); len(f) != 0 {
		t.Fatal("filled against a print from before the order reached the exchange")
	}
}

func TestAMakerOrderThatNeverFillsIsCancelledAndSaysSo(t *testing.T) {
	t.Parallel()
	// The most important output of the whole simulator: the trades a passive
	// strategy simply does not get.
	s := sim(func(c *Config) { c.MakerTimeout = 5 * time.Second })
	bids, asks := book()
	o, _ := s.Submit(openLong(), Maker, snap(), marketstate.Position{}, now0)
	s.SeedQueue(o, bids, asks)

	_, cancels := s.OnBook(bids, asks, now0.Add(10*time.Second))
	if len(cancels) != 1 {
		t.Fatalf("cancels = %d, want 1", len(cancels))
	}
	if cancels[0].Unfilled <= 0 {
		t.Error("a cancel must carry what was left unfilled")
	}
	if s.Working() != 0 {
		t.Error("a cancelled order is not still working")
	}
}

func TestShortEntryIsRefusedOnSpotRatherThanQuietlyFilled(t *testing.T) {
	t.Parallel()
	// decide.Compose emits IntentOpenShort because the model is asked a
	// symmetric question. Spot has no borrow, so this position could never
	// have existed and must not appear in the dataset.
	s := sim()
	sig := decide.Signal{At: now0, Intent: decide.IntentOpenShort}
	o, err := s.Submit(sig, Taker, snap(), marketstate.Position{}, now0)
	if !errors.Is(err, ErrShortOnSpot) {
		t.Fatalf("err = %v, want ErrShortOnSpot", err)
	}
	if o != nil {
		t.Error("an unexecutable intent must not produce an order")
	}

	// With margin it would be a normal sell, so the refusal is configuration
	// and not a hardcoded belief about the model.
	s2 := sim(func(c *Config) { c.AllowShort = true })
	if o, err := s2.Submit(sig, Taker, snap(), marketstate.Position{}, now0); err != nil || o == nil {
		t.Fatalf("AllowShort did not permit the short: o=%v err=%v", o, err)
	}
}

func TestClosingSellsWhatIsHeldRatherThanTheConfiguredSize(t *testing.T) {
	t.Parallel()
	s := sim(func(c *Config) { c.NotionalJPY = 1e9 })
	bids, asks := book()
	pos := marketstate.Position{Side: "long", Size: 7, EntryPrice: 207.00, OpenedAt: now0}

	o, err := s.Submit(decide.Signal{Intent: decide.IntentClose}, Taker, snap(), pos, now0)
	if err != nil {
		t.Fatal(err)
	}
	if o.Side != "sell" || o.Size != 7 {
		t.Fatalf("close order = %s %v, want sell 7", o.Side, o.Size)
	}
	fills, _ := s.OnBook(bids, asks, now0.Add(time.Second))
	if len(fills) != 1 || fills[0].Size != 7 {
		t.Fatalf("close filled %v", fills)
	}
}

func TestClosingNothingProducesNoOrder(t *testing.T) {
	t.Parallel()
	s := sim()
	o, err := s.Submit(decide.Signal{Intent: decide.IntentClose}, Taker, snap(), marketstate.Position{}, now0)
	if err != nil || o != nil {
		t.Fatalf("o=%v err=%v, want no order and no error", o, err)
	}
}

// wholeJSON and txJSON build the raw frames a Book ingests, so the trader
// tests exercise the real parsing path rather than a hand-built book.
func wholeJSON() []byte {
	return []byte(`{"asks":[["207.52","80"],["207.53","300"],["207.54","700"]],` +
		`"bids":[["207.50","100"],["207.49","400"],["207.48","900"]],` +
		`"timestamp":1789200000000,"sequenceId":"1"}`)
}

func txJSON(id int64, at time.Time, side string, price, amount float64) []byte {
	return []byte(fmt.Sprintf(
		`{"transactions":[{"transaction_id":%d,"side":%q,"price":"%g","amount":"%g","executed_at":%d}]}`,
		id, side, price, amount, at.UnixMilli()))
}
