package bitbank

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// Order status values bitbank reports. Anything not UNFILLED or
// PARTIALLY_FILLED is finished and will never fill again.
const (
	StatusInactive         = "INACTIVE"
	StatusUnfilled         = "UNFILLED"
	StatusPartiallyFilled  = "PARTIALLY_FILLED"
	StatusFullyFilled      = "FULLY_FILLED"
	StatusCanceledUnfilled = "CANCELED_UNFILLED"
	StatusCanceledPartial  = "CANCELED_PARTIALLY_FILLED"
)

// Order is one order as the exchange sees it.
//
// Every numeric field arrives as a string. They are kept as strings and
// converted explicitly, because a float64 of an amount is a rounding error
// waiting to be sent back as an order size.
type Order struct {
	OrderID         int64  `json:"order_id"`
	Pair            string `json:"pair"`
	Side            string `json:"side"`
	Type            string `json:"type"`
	StartAmount     string `json:"start_amount"`
	RemainingAmount string `json:"remaining_amount"`
	ExecutedAmount  string `json:"executed_amount"`
	Price           string `json:"price"`
	PostOnly        bool   `json:"post_only"`
	AveragePrice    string `json:"average_price"`
	OrderedAt       int64  `json:"ordered_at"`
	Status          string `json:"status"`
}

// Done reports whether this order can no longer fill.
func (o Order) Done() bool {
	switch o.Status {
	case StatusUnfilled, StatusPartiallyFilled, StatusInactive:
		return false
	}
	return true
}

func (o Order) Executed() float64  { return num(o.ExecutedAmount) }
func (o Order) Remaining() float64 { return num(o.RemainingAmount) }
func (o Order) AvgPrice() float64  { return num(o.AveragePrice) }

// Num parses one of bitbank's string-encoded numbers. Exported because the
// live executor reads prices back off an Order.
func Num(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

func num(s string) float64 { return Num(s) }

// NewOrder is a request to place one order.
//
// Amount and Price are strings for the same reason the responses are: they
// must be sent at the pair's exact precision, and formatting a float at the
// point of sending is how an order gets rejected for 0.30000000000000004.
// Use PairRules.FormatAmount and FormatPrice.
type NewOrder struct {
	Pair   string `json:"pair"`
	Amount string `json:"amount"`
	Price  string `json:"price,omitempty"`
	Side   string `json:"side"`
	Type   string `json:"type"`

	// PostOnly makes a limit order maker-or-cancel. Without it a limit order
	// priced through the book crosses and pays the taker fee — which quietly
	// turns the cheap path into the expensive one.
	PostOnly bool `json:"post_only,omitempty"`
}

// PlaceOrder submits an order.
//
// It never retries. A POST that times out is ambiguous — the order may well
// have reached the exchange — and retrying an ambiguous order placement is how
// one signal becomes three positions. The caller reconciles instead.
func (c *Client) PlaceOrder(ctx context.Context, o NewOrder) (Order, error) {
	if o.Type != "limit" && o.Type != "market" {
		return Order{}, fmt.Errorf("bitbank: order type %q is not supported here; this experiment places limit and market orders only", o.Type)
	}
	var out Order
	if err := c.post(ctx, "/user/spot/order", o, &out); err != nil {
		return Order{}, err
	}
	return out, nil
}

// CancelOrder withdraws a working order. A 50008/50009 means it was already
// finished, which is a normal race and not an error the caller must handle.
func (c *Client) CancelOrder(ctx context.Context, pair string, id int64) (Order, error) {
	var out Order
	body := struct {
		Pair    string `json:"pair"`
		OrderID int64  `json:"order_id"`
	}{pair, id}
	if err := c.post(ctx, "/user/spot/cancel_order", body, &out); err != nil {
		return Order{}, err
	}
	return out, nil
}

// GetOrder fetches one order's current state.
func (c *Client) GetOrder(ctx context.Context, pair string, id int64) (Order, error) {
	q := url.Values{"pair": {pair}, "order_id": {strconv.FormatInt(id, 10)}}
	var out Order
	if err := c.get(ctx, "/user/spot/order", q, &out); err != nil {
		return Order{}, err
	}
	return out, nil
}

// ActiveOrders is every working order on a pair.
//
// This is half of the startup reconciliation CLAUDE.md rule 7 requires: a
// crash-restart that trusts a local file will happily place a second order
// alongside one it forgot about.
func (c *Client) ActiveOrders(ctx context.Context, pair string) ([]Order, error) {
	q := url.Values{"pair": {pair}}
	var out struct {
		Orders []Order `json:"orders"`
	}
	if err := c.get(ctx, "/user/spot/active_orders", q, &out); err != nil {
		return nil, err
	}
	return out.Orders, nil
}

// Asset is one currency's balance.
type Asset struct {
	Asset           string `json:"asset"`
	FreeAmount      string `json:"free_amount"`
	LockedAmount    string `json:"locked_amount"`
	OnhandAmount    string `json:"onhand_amount"`
	AmountPrecision int    `json:"amount_precision"`
}

func (a Asset) Free() float64   { return num(a.FreeAmount) }
func (a Asset) Onhand() float64 { return num(a.OnhandAmount) }

// Assets is every balance on the account. The other half of reconciliation:
// what the exchange says is actually held.
func (c *Client) Assets(ctx context.Context) ([]Asset, error) {
	var out struct {
		Assets []Asset `json:"assets"`
	}
	if err := c.get(ctx, "/user/assets", nil, &out); err != nil {
		return nil, err
	}
	return out.Assets, nil
}

// Asset picks one currency out of a balance list.
func FindAsset(list []Asset, name string) (Asset, bool) {
	for _, a := range list {
		if a.Asset == name {
			return a, true
		}
	}
	return Asset{}, false
}
