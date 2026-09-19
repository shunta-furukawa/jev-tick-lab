package bitbank

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PairRules is what the exchange says about a pair right now.
//
// Fetched, not configured. CLAUDE.md requires fee rates to be configuration
// rather than constants, because the bitbank maker rebate is a campaign that
// can end — but reading them from the venue is strictly better than
// configuring them, because a config file can be stale and this cannot. The
// same call carries the minimum order size, the rounding precision, and
// whether the pair is accepting orders at all.
type PairRules struct {
	Name         string
	MakerFeeBps  float64
	TakerFeeBps  float64
	UnitAmount   float64 // minimum order size, in base units
	MaxAmount    float64
	PriceDigits  int
	AmountDigits int

	Enabled          bool
	StopOrder        bool
	StopOrderAndCanc bool
	StopMarketOrder  bool
	FetchedAt        time.Time
}

// TradingAllowed reports whether an order can be placed at all. A suspended
// pair is a fact off the wire, so it is checked rather than asked about.
func (p PairRules) TradingAllowed() bool {
	return p.Enabled && !p.StopOrder && !p.StopOrderAndCanc
}

// FormatAmount renders a size at the pair's precision, rounding DOWN.
//
// Down, never nearest: rounding a sell up sells base the account does not
// have, and rounding a buy up spends quote it does not have. Both come back as
// an opaque insufficient-funds error.
func (p PairRules) FormatAmount(v float64) string {
	return strconv.FormatFloat(floorTo(v, p.AmountDigits), 'f', p.AmountDigits, 64)
}

// FormatPrice renders a price at the pair's precision. A limit price is
// rounded toward the passive side by the caller, not here.
func (p PairRules) FormatPrice(v float64) string {
	return strconv.FormatFloat(roundTo(v, p.PriceDigits), 'f', p.PriceDigits, 64)
}

// RoundPassive moves a limit price to the pair's tick, away from the mid, so
// the order stays on the side of the book it was meant for. Rounding a buy
// limit up can cross the spread and turn a maker order into a taker one.
func (p PairRules) RoundPassive(v float64, side string) float64 {
	if side == "buy" {
		return floorTo(v, p.PriceDigits)
	}
	return ceilTo(v, p.PriceDigits)
}

func pow10(n int) float64 { return math.Pow(10, float64(n)) }

func floorTo(v float64, digits int) float64 {
	f := pow10(digits)
	// The nudge absorbs binary representation error: 0.3*10000 is
	// 2999.9999999999995, and flooring that loses a whole unit.
	return math.Floor(v*f+1e-9) / f
}

func ceilTo(v float64, digits int) float64 {
	f := pow10(digits)
	return math.Ceil(v*f-1e-9) / f
}

func roundTo(v float64, digits int) float64 {
	f := pow10(digits)
	return math.Round(v*f) / f
}

type rawPair struct {
	Name               string `json:"name"`
	MakerFeeRateQuote  string `json:"maker_fee_rate_quote"`
	TakerFeeRateQuote  string `json:"taker_fee_rate_quote"`
	UnitAmount         string `json:"unit_amount"`
	LimitMaxAmount     string `json:"limit_max_amount"`
	PriceDigits        int    `json:"price_digits"`
	AmountDigits       int    `json:"amount_digits"`
	IsEnabled          bool   `json:"is_enabled"`
	StopOrder          bool   `json:"stop_order"`
	StopOrderAndCancel bool   `json:"stop_order_and_cancel"`
	StopMarketOrder    bool   `json:"stop_market_order"`
}

// FetchPairRules reads the pair table. It needs no authentication, so it can
// be run — and should be — long before any key exists.
func FetchPairRules(ctx context.Context, endpoint, pair string) (PairRules, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/spot/pairs", nil)
	if err != nil {
		return PairRules{}, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return PairRules{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return PairRules{}, err
	}
	var env struct {
		Success int `json:"success"`
		Data    struct {
			Pairs []rawPair `json:"pairs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return PairRules{}, fmt.Errorf("bitbank pairs: %w", err)
	}
	if env.Success != 1 {
		return PairRules{}, fmt.Errorf("bitbank pairs: success=%d", env.Success)
	}
	for _, p := range env.Data.Pairs {
		if !strings.EqualFold(p.Name, pair) {
			continue
		}
		return PairRules{
			Name:             p.Name,
			MakerFeeBps:      num(p.MakerFeeRateQuote) * 10000,
			TakerFeeBps:      num(p.TakerFeeRateQuote) * 10000,
			UnitAmount:       num(p.UnitAmount),
			MaxAmount:        num(p.LimitMaxAmount),
			PriceDigits:      p.PriceDigits,
			AmountDigits:     p.AmountDigits,
			Enabled:          p.IsEnabled,
			StopOrder:        p.StopOrder,
			StopOrderAndCanc: p.StopOrderAndCancel,
			StopMarketOrder:  p.StopMarketOrder,
			FetchedAt:        time.Now().UTC(),
		}, nil
	}
	return PairRules{}, fmt.Errorf("bitbank pairs: %q is not in the pair table", pair)
}
