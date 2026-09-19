package risk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/bitbank"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// Exchange is the slice of bitbank this package needs. An interface so the
// reconciliation logic is testable without a network or a key — the thing that
// must work on the worst day is the thing that most needs a test.
type Exchange interface {
	Assets(ctx context.Context) ([]bitbank.Asset, error)
	ActiveOrders(ctx context.Context, pair string) ([]bitbank.Order, error)
	CancelOrder(ctx context.Context, pair string, id int64) (bitbank.Order, error)
}

// Reconciliation is what the exchange says we actually have.
type Reconciliation struct {
	Position  marketstate.Position
	BaseFree  float64
	QuoteFree float64

	// Cancelled lists the working orders that were withdrawn on startup.
	Cancelled []int64

	// Notes are things a human should read before the bot trades. They are
	// warnings, not errors: the run continues, but with this on the record.
	Notes []string
}

// Reconcile establishes the truth before the first order.
//
// CLAUDE.md rule 7: position state is never trusted from local storage. A
// crash-restart that believes a stale file will place a second entry alongside
// a position it forgot about, and on a spot account that shows up as a balance
// quietly twice the intended size.
//
// It does three things, in this order:
//
//  1. Cancels every working order on the pair. The bot cannot adopt orders it
//     has no record of — it does not know what they were for, and leaving them
//     resting means an unknown instruction can fill at any moment.
//  2. Reads the balance, which is what a spot position actually is.
//  3. Reports what it found, including the awkward cases, rather than
//     normalising them away.
//
// A spot balance is not a position: it cannot say what was paid for it. Entry
// price is left zero and the caller must treat an adopted balance as a holding
// of unknown cost, never as a round trip in progress.
func Reconcile(ctx context.Context, ex Exchange, pair string, dustBase float64) (Reconciliation, error) {
	base, quote, err := splitPair(pair)
	if err != nil {
		return Reconciliation{}, err
	}

	var r Reconciliation

	orders, err := ex.ActiveOrders(ctx, pair)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile: read working orders: %w", err)
	}
	for _, o := range orders {
		if o.Done() {
			continue
		}
		if _, err := ex.CancelOrder(ctx, pair, o.OrderID); err != nil {
			// Refuse to start rather than trade alongside an order we could
			// not withdraw. Whatever it is, it can still fill.
			return Reconciliation{}, fmt.Errorf("reconcile: could not cancel working order %d: %w", o.OrderID, err)
		}
		r.Cancelled = append(r.Cancelled, o.OrderID)
	}
	if n := len(r.Cancelled); n > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"cancelled %d working order(s) left from a previous run; the bot cannot adopt orders it has no record of", n))
	}

	assets, err := ex.Assets(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile: read balances: %w", err)
	}
	if a, found := bitbank.FindAsset(assets, base); found {
		r.BaseFree = a.Free()
		if locked := a.Onhand() - a.Free(); locked > dustBase {
			r.Notes = append(r.Notes, fmt.Sprintf(
				"%.4f %s is locked and not tradeable here — another order or a withdrawal elsewhere", locked, base))
		}
	}
	if a, found := bitbank.FindAsset(assets, quote); found {
		r.QuoteFree = a.Free()
	}

	if r.BaseFree > dustBase {
		r.Position = marketstate.Position{
			Side: "long",
			Size: r.BaseFree,
			// Deliberately zero. The exchange knows the balance, not what was
			// paid for it, and inventing an entry price would produce a P&L
			// that is fiction.
			EntryPrice: 0,
			OpenedAt:   time.Now().UTC(),
		}
		r.Notes = append(r.Notes, fmt.Sprintf(
			"adopted %.4f %s already in the account as an open position of unknown cost; "+
				"its P&L is not attributable to this run", r.BaseFree, base))
	}
	return r, nil
}

// splitPair turns "xrp_jpy" into its base and quote assets.
func splitPair(pair string) (base, quote string, err error) {
	parts := strings.Split(pair, "_")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("reconcile: %q is not a bitbank pair", pair)
	}
	return parts[0], parts[1], nil
}
