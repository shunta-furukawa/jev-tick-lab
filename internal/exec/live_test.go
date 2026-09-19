package exec

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/bitbank"
	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/risk"
)

type fakeVenue struct {
	placed    []bitbank.NewOrder
	cancelled []int64
	next      bitbank.Order
	placeErr  error
	getErr    error
	cancelErr error
	seq       int64
}

func (v *fakeVenue) PlaceOrder(_ context.Context, o bitbank.NewOrder) (bitbank.Order, error) {
	if v.placeErr != nil {
		return bitbank.Order{}, v.placeErr
	}
	v.placed = append(v.placed, o)
	v.seq++
	out := bitbank.Order{OrderID: v.seq, Pair: o.Pair, Side: o.Side, Type: o.Type,
		Price: o.Price, StartAmount: o.Amount, RemainingAmount: o.Amount,
		ExecutedAmount: "0", Status: bitbank.StatusUnfilled}
	v.next = out
	return out, nil
}

func (v *fakeVenue) CancelOrder(_ context.Context, _ string, id int64) (bitbank.Order, error) {
	if v.cancelErr != nil {
		return bitbank.Order{}, v.cancelErr
	}
	v.cancelled = append(v.cancelled, id)
	return bitbank.Order{OrderID: id, Status: bitbank.StatusCanceledUnfilled}, nil
}

func (v *fakeVenue) GetOrder(context.Context, string, int64) (bitbank.Order, error) {
	return v.next, v.getErr
}

func liveRules() bitbank.PairRules {
	return bitbank.PairRules{
		Name: "xrp_jpy", MakerFeeBps: -2, TakerFeeBps: 12,
		UnitAmount: 0.0001, PriceDigits: 3, AmountDigits: 4, Enabled: true,
	}
}

func newLive(t *testing.T, style Style) (*Live, *fakeVenue) {
	t.Helper()
	v := &fakeVenue{}
	cfg := DefaultLiveConfig("xrp_jpy", liveRules())
	cfg.Style = style
	l := NewLive(v, cfg)
	l.Adopt(marketstate.Position{})
	return l, v
}

func TestOnlyOneOrderIsEverOutstanding(t *testing.T) {
	t.Parallel()
	// A second order placed on a stale decision is two positions where there
	// should be one. The tick loop skips rather than queues, exactly as it
	// does for a slow model call.
	l, _ := newLive(t, Taker)
	if _, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000); err != nil {
		t.Fatal(err)
	}
	_, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestExitOnlyRefusesEntriesAndStillAllowsExits(t *testing.T) {
	t.Parallel()
	// A brake that traps a position is not a brake.
	l, v := newLive(t, Taker)

	o, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.ExitOnly, 3000)
	if err != nil || o != nil {
		t.Fatalf("exit-only placed an entry: o=%v err=%v", o, err)
	}

	l.Adopt(marketstate.Position{Side: "long", Size: 13.4567, EntryPrice: 207})
	o, err = l.Submit(context.Background(), now0, decide.Signal{Intent: decide.IntentClose}, snap(), risk.ExitOnly, 3000)
	if err != nil || o == nil {
		t.Fatalf("exit-only blocked a close: o=%v err=%v", o, err)
	}
	if v.placed[0].Side != "sell" || v.placed[0].Amount != "13.4567" {
		t.Errorf("close order = %+v", v.placed[0])
	}
}

func TestHaltPlacesNothingInEitherDirection(t *testing.T) {
	t.Parallel()
	l, v := newLive(t, Taker)
	l.Adopt(marketstate.Position{Side: "long", Size: 10, EntryPrice: 207})
	for _, sig := range []decide.Signal{openLong(), {Intent: decide.IntentClose}} {
		if o, err := l.Submit(context.Background(), now0, sig, snap(), risk.Halt, 3000); o != nil || err != nil {
			t.Errorf("halt placed %v (%v)", o, err)
		}
	}
	if len(v.placed) != 0 {
		t.Errorf("orders reached the venue under halt: %v", v.placed)
	}
}

func TestAMakerEntryIsPostOnlyAndRestsOnItsOwnSide(t *testing.T) {
	t.Parallel()
	// Without post_only a limit priced through the book crosses and pays the
	// taker fee, turning the -2bps path into the +12bps one with nothing in
	// the logs to say so.
	l, v := newLive(t, Maker)
	if _, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000); err != nil {
		t.Fatal(err)
	}
	o := v.placed[0]
	if o.Type != "limit" || !o.PostOnly {
		t.Errorf("maker order = %+v, want a post-only limit", o)
	}
	// snap() has bid 207.50 / ask 207.52. A buy must rest at the bid.
	if o.Price != "207.500" {
		t.Errorf("buy rested at %s, want the bid at 207.500", o.Price)
	}
}

func TestAShortIsRefusedEvenThoughTheModelAskedForIt(t *testing.T) {
	t.Parallel()
	// The same rule the simulator has, enforced again here because this one
	// spends money.
	l, v := newLive(t, Taker)
	_, err := l.Submit(context.Background(), now0, decide.Signal{Intent: decide.IntentOpenShort}, snap(), risk.Trade, 3000)
	if !errors.Is(err, ErrShortOnSpot) {
		t.Fatalf("err = %v, want ErrShortOnSpot", err)
	}
	if len(v.placed) != 0 {
		t.Error("a short reached the exchange")
	}
}

func TestAPartialFillPolledTwiceIsBookedOnce(t *testing.T) {
	t.Parallel()
	// Double-booking a partial turns one position into an imaginary two and
	// corrupts every P&L after it.
	l, v := newLive(t, Taker)
	if _, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000); err != nil {
		t.Fatal(err)
	}

	v.next = bitbank.Order{OrderID: 1, Side: "buy", ExecutedAmount: "5",
		RemainingAmount: "9.4", AveragePrice: "207.52", Status: bitbank.StatusPartiallyFilled}

	first, err := l.Poll(context.Background(), now0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Fill.Size != 5 {
		t.Fatalf("first poll = %+v, want one fill of 5", first)
	}

	// Nothing new since.
	again, err := l.Poll(context.Background(), now0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("the same partial was booked twice: %+v", again)
	}

	// Now the rest fills: only the increment is booked.
	v.next.ExecutedAmount, v.next.RemainingAmount, v.next.Status = "14.4", "0", bitbank.StatusFullyFilled
	rest, err := l.Poll(context.Background(), now0.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || math.Abs(rest[0].Fill.Size-9.4) > 1e-9 {
		t.Fatalf("increment = %+v, want 9.4", rest)
	}
	if got := l.Position().Size; math.Abs(got-14.4) > 1e-9 {
		t.Errorf("position = %v, want 14.4", got)
	}
	if l.Working() {
		t.Error("a fully filled order is still marked working")
	}
}

func TestAFillWithNoPublishedPriceIsNotInvented(t *testing.T) {
	t.Parallel()
	// A market order that has filled but has no average price yet must wait,
	// not be booked at zero — which would record a free position.
	l, v := newLive(t, Taker)
	l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000)

	v.next = bitbank.Order{OrderID: 1, Side: "buy", ExecutedAmount: "5",
		AveragePrice: "", Status: bitbank.StatusPartiallyFilled}
	got, err := l.Poll(context.Background(), now0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("booked a fill with no price: %+v", got)
	}

	v.next.AveragePrice = "207.52"
	got, err = l.Poll(context.Background(), now0.Add(2*time.Second))
	if err != nil || len(got) != 1 {
		t.Fatalf("the fill was lost once the price arrived: %+v (%v)", got, err)
	}
}

func TestAnUnfilledOrderIsCancelledAtTheTimeout(t *testing.T) {
	t.Parallel()
	l, v := newLive(t, Maker)
	l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000)
	v.next = bitbank.Order{OrderID: 1, Side: "buy", ExecutedAmount: "0", Status: bitbank.StatusUnfilled}

	if _, err := l.Poll(context.Background(), now0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if l.Working() != true {
		t.Fatal("cancelled before the timeout")
	}
	if _, err := l.Poll(context.Background(), now0.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(v.cancelled) != 1 {
		t.Errorf("cancelled = %v, want one", v.cancelled)
	}
	if l.Working() {
		t.Error("still working after the cancel")
	}
}

func TestAFailedCancelLeavesTheOrderMarkedWorking(t *testing.T) {
	t.Parallel()
	// If the order could not be withdrawn it can still fill, and pretending
	// otherwise is how the bot places a second one alongside it.
	l, v := newLive(t, Maker)
	l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000)
	v.next = bitbank.Order{OrderID: 1, Side: "buy", ExecutedAmount: "0", Status: bitbank.StatusUnfilled}
	v.cancelErr = errors.New("network down")

	if _, err := l.Poll(context.Background(), now0.Add(31*time.Second)); err == nil {
		t.Fatal("a failed cancel was reported as success")
	}
	if !l.Working() {
		t.Error("the order was forgotten despite the cancel failing")
	}
}

func TestAnAlreadyFinishedCancelIsNotAnError(t *testing.T) {
	t.Parallel()
	// The common race: it filled between the poll and the cancel.
	l, v := newLive(t, Maker)
	l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000)
	v.next = bitbank.Order{OrderID: 1, Side: "buy", ExecutedAmount: "0", Status: bitbank.StatusUnfilled}
	v.cancelErr = &bitbank.APIError{Code: 50009}

	if _, err := l.Poll(context.Background(), now0.Add(31*time.Second)); err != nil {
		t.Fatalf("err = %v, want nil for an already-finished order", err)
	}
}

func TestClosingDustFlattensRatherThanRetryingForever(t *testing.T) {
	t.Parallel()
	// A remainder below the venue minimum can never be sold. Sending it every
	// tick produces an endless stream of rejections.
	l, v := newLive(t, Taker)
	l.Adopt(marketstate.Position{Side: "long", Size: 0.00001, EntryPrice: 207})

	o, err := l.Submit(context.Background(), now0, decide.Signal{Intent: decide.IntentClose}, snap(), risk.Trade, 3000)
	if err != nil || o != nil {
		t.Fatalf("o=%v err=%v, want the dust written off quietly", o, err)
	}
	if !l.Position().IsFlat() {
		t.Error("dust still counted as a position")
	}
	if len(v.placed) != 0 {
		t.Error("an unsellable order was sent")
	}
}

func TestAnEntryBelowTheVenueMinimumIsAnErrorNotADustWriteOff(t *testing.T) {
	t.Parallel()
	// The opposite of the dust case: too small to buy means the configuration
	// is wrong and should say so, loudly.
	l, _ := newLive(t, Taker)
	if _, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 0.001); err == nil {
		t.Fatal("a sub-minimum entry was accepted")
	}
}

func TestASuspendedPairIsRefusedBeforeAnOrderIsSent(t *testing.T) {
	t.Parallel()
	v := &fakeVenue{}
	cfg := DefaultLiveConfig("xrp_jpy", liveRules())
	cfg.Rules.StopOrder = true
	l := NewLive(v, cfg)

	if _, err := l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000); err == nil {
		t.Fatal("placed an order on a suspended pair")
	}
	if len(v.placed) != 0 {
		t.Error("the order reached the venue")
	}
}

func TestSizeIsSentAtTheVenuePrecision(t *testing.T) {
	t.Parallel()
	// 3000 JPY at a mid of 207.51 is 14.457134..., which must be sent as
	// 14.4571 — four digits, rounded down — not as a float the exchange
	// rejects or a size the balance cannot cover.
	l, v := newLive(t, Taker)
	l.Submit(context.Background(), now0, openLong(), snap(), risk.Trade, 3000)
	if got := v.placed[0].Amount; got != "14.4571" {
		t.Errorf("amount = %q, want 14.4571 at four digits, rounded down", got)
	}
}
