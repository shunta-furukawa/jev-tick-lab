package bitbank

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func xrpRules() PairRules {
	// The real xrp_jpy values, read from GET /spot/pairs on 2026-09-19.
	return PairRules{
		Name: "xrp_jpy", MakerFeeBps: -2, TakerFeeBps: 12,
		UnitAmount: 0.0001, PriceDigits: 3, AmountDigits: 4,
		Enabled: true,
	}
}

func TestAmountRoundsDownSoAnOrderNeverExceedsTheBalance(t *testing.T) {
	t.Parallel()
	r := xrpRules()
	// Rounding a sell up sells base the account does not hold; rounding a buy
	// up spends quote it does not have. Both come back as an opaque
	// insufficient-funds error at the worst moment.
	if got := r.FormatAmount(13.456789); got != "13.4567" {
		t.Errorf("FormatAmount(13.456789) = %q, want 13.4567", got)
	}
	// And binary representation error must not eat a whole unit: 0.3 at four
	// digits is 0.3000, not 0.2999.
	if got := r.FormatAmount(0.3); got != "0.3000" {
		t.Errorf("FormatAmount(0.3) = %q, want 0.3000", got)
	}
}

func TestPriceIsRoundedAwayFromTheMidSoAMakerStaysAMaker(t *testing.T) {
	t.Parallel()
	r := xrpRules()
	// A buy limit rounded UP can cross the spread, which silently turns the
	// -2bps path into the +12bps one.
	if got := r.RoundPassive(223.0409, "buy"); math.Abs(got-223.040) > 1e-9 {
		t.Errorf("buy limit rounded to %v, want 223.040 (down)", got)
	}
	if got := r.RoundPassive(223.0401, "sell"); math.Abs(got-223.041) > 1e-9 {
		t.Errorf("sell limit rounded to %v, want 223.041 (up)", got)
	}
	if got := r.FormatPrice(223.0409); got != "223.041" {
		t.Errorf("FormatPrice = %q", got)
	}
}

func TestASuspendedPairIsNotTradeable(t *testing.T) {
	t.Parallel()
	r := xrpRules()
	if !r.TradingAllowed() {
		t.Fatal("a normal pair should be tradeable")
	}
	for _, mut := range []func(*PairRules){
		func(p *PairRules) { p.Enabled = false },
		func(p *PairRules) { p.StopOrder = true },
		func(p *PairRules) { p.StopOrderAndCanc = true },
	} {
		p := xrpRules()
		mut(&p)
		if p.TradingAllowed() {
			t.Errorf("a suspended pair reported tradeable: %+v", p)
		}
	}
}

func TestFeesComeFromTheExchangeRatherThanAConstant(t *testing.T) {
	t.Parallel()
	// The rebate is a campaign. When it ends, the venue's own table is the
	// thing that changes, and this must follow it without a code change.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spot/pairs" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Write([]byte(`{"success":1,"data":{"pairs":[
			{"name":"xrp_jpy","maker_fee_rate_quote":"0.0005","taker_fee_rate_quote":"0.0015",
			 "unit_amount":"0.0001","limit_max_amount":"40000000",
			 "price_digits":3,"amount_digits":4,"is_enabled":true,
			 "stop_order":false,"stop_order_and_cancel":false,"stop_market_order":false}]}}`))
	}))
	defer srv.Close()

	got, err := FetchPairRules(context.Background(), srv.URL, "xrp_jpy")
	if err != nil {
		t.Fatal(err)
	}
	if got.MakerFeeBps != 5 || got.TakerFeeBps != 15 {
		t.Errorf("fees = %v/%v bps, want the venue's 5/15 — not a hardcoded -2/12",
			got.MakerFeeBps, got.TakerFeeBps)
	}
	if got.UnitAmount != 0.0001 || got.PriceDigits != 3 {
		t.Errorf("rules = %+v", got)
	}
}

func TestAnUnknownPairIsAnErrorRatherThanZeroFees(t *testing.T) {
	t.Parallel()
	// Falling back to a zero-valued PairRules would report a free venue with a
	// zero minimum size, which is the most dangerous possible default.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":1,"data":{"pairs":[]}}`))
	}))
	defer srv.Close()

	if _, err := FetchPairRules(context.Background(), srv.URL, "xrp_jpy"); err == nil {
		t.Fatal("an unknown pair returned no error")
	}
}
