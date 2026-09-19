package risk

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shunta-furukawa/jev-tick-lab/internal/bitbank"
)

type fakeExchange struct {
	assets    []bitbank.Asset
	orders    []bitbank.Order
	cancelled []int64
	cancelErr error
	assetsErr error
}

func (f *fakeExchange) Assets(context.Context) ([]bitbank.Asset, error) {
	return f.assets, f.assetsErr
}
func (f *fakeExchange) ActiveOrders(context.Context, string) ([]bitbank.Order, error) {
	return f.orders, nil
}
func (f *fakeExchange) CancelOrder(_ context.Context, _ string, id int64) (bitbank.Order, error) {
	if f.cancelErr != nil {
		return bitbank.Order{}, f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	return bitbank.Order{OrderID: id, Status: bitbank.StatusCanceledUnfilled}, nil
}

func TestABalanceAlreadyHeldIsAdoptedNotIgnored(t *testing.T) {
	t.Parallel()
	// The restart case CLAUDE.md rule 7 exists for. Starting flat while the
	// account holds XRP means the next entry doubles the position.
	ex := &fakeExchange{assets: []bitbank.Asset{
		{Asset: "xrp", FreeAmount: "13.4567", OnhandAmount: "13.4567"},
		{Asset: "jpy", FreeAmount: "5000", OnhandAmount: "5000"},
	}}

	r, err := Reconcile(context.Background(), ex, "xrp_jpy", 0.001)
	if err != nil {
		t.Fatal(err)
	}
	if r.Position.IsFlat() || r.Position.Size != 13.4567 {
		t.Fatalf("position = %+v, want the 13.4567 XRP the exchange reports", r.Position)
	}
	if r.QuoteFree != 5000 {
		t.Errorf("quote free = %v, want 5000", r.QuoteFree)
	}
	// Entry price must stay zero: a balance cannot say what was paid for it,
	// and an invented entry price produces a fictitious P&L.
	if r.Position.EntryPrice != 0 {
		t.Errorf("entry price = %v; the exchange does not know it", r.Position.EntryPrice)
	}
	if !hasNote(r.Notes, "unknown cost") {
		t.Errorf("no warning that the adopted position has no cost basis: %v", r.Notes)
	}
}

func TestWorkingOrdersFromAPreviousRunAreCancelled(t *testing.T) {
	t.Parallel()
	// An order the bot has no record of is an instruction it cannot reason
	// about that can fill at any moment.
	ex := &fakeExchange{orders: []bitbank.Order{
		{OrderID: 11, Status: bitbank.StatusUnfilled},
		{OrderID: 12, Status: bitbank.StatusPartiallyFilled},
		{OrderID: 13, Status: bitbank.StatusFullyFilled}, // already done
	}}

	r, err := Reconcile(context.Background(), ex, "xrp_jpy", 0.001)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Cancelled) != 2 {
		t.Fatalf("cancelled %v, want the two working orders", r.Cancelled)
	}
	if len(ex.cancelled) != 2 {
		t.Errorf("the exchange saw %v", ex.cancelled)
	}
}

func TestAnUncancellableOrderRefusesToStart(t *testing.T) {
	t.Parallel()
	// Trading alongside an order that could not be withdrawn is trading
	// against yourself with an unknown size.
	ex := &fakeExchange{
		orders:    []bitbank.Order{{OrderID: 11, Status: bitbank.StatusUnfilled}},
		cancelErr: errors.New("bitbank error 50009"),
	}
	if _, err := Reconcile(context.Background(), ex, "xrp_jpy", 0.001); err == nil {
		t.Fatal("started despite an order it could not cancel")
	}
}

func TestDustIsNotAPosition(t *testing.T) {
	t.Parallel()
	// A few thousandths of XRP left from rounding is not something to sell,
	// and treating it as a position blocks every entry forever.
	ex := &fakeExchange{assets: []bitbank.Asset{
		{Asset: "xrp", FreeAmount: "0.0002", OnhandAmount: "0.0002"},
	}}
	r, err := Reconcile(context.Background(), ex, "xrp_jpy", 0.001)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Position.IsFlat() {
		t.Errorf("dust adopted as a position: %+v", r.Position)
	}
}

func TestLockedBalanceIsReportedRatherThanCountedAsTradeable(t *testing.T) {
	t.Parallel()
	ex := &fakeExchange{assets: []bitbank.Asset{
		{Asset: "xrp", FreeAmount: "5", OnhandAmount: "12"},
	}}
	r, err := Reconcile(context.Background(), ex, "xrp_jpy", 0.001)
	if err != nil {
		t.Fatal(err)
	}
	if r.Position.Size != 5 {
		t.Errorf("position = %v, want only the free 5", r.Position.Size)
	}
	if !hasNote(r.Notes, "locked") {
		t.Errorf("the 7 locked XRP were not mentioned: %v", r.Notes)
	}
}

func TestAFailedBalanceReadIsAnErrorNotAFlatPosition(t *testing.T) {
	t.Parallel()
	// Defaulting to flat when the exchange cannot be read is the single most
	// dangerous fallback available here.
	ex := &fakeExchange{assetsErr: errors.New("timeout")}
	if _, err := Reconcile(context.Background(), ex, "xrp_jpy", 0.001); err == nil {
		t.Fatal("a failed balance read produced a flat position")
	}
}

func TestAMalformedPairIsRejected(t *testing.T) {
	t.Parallel()
	if _, err := Reconcile(context.Background(), &fakeExchange{}, "xrpjpy", 0.001); err == nil {
		t.Fatal("accepted a pair with no separator")
	}
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
