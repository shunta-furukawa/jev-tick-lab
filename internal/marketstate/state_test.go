package marketstate

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func whole(seq uint64, ts time.Time, bids, asks [][2]string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"bids": bids, "asks": asks,
		"timestamp":  ts.UnixMilli(),
		"sequenceId": fmt.Sprint(seq), // bitbank sends this as a string
	})
	if err != nil {
		panic(err)
	}
	return b
}

func diff(seq uint64, ts time.Time, bids, asks [][2]string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"b": bids, "a": asks,
		"t": ts.UnixMilli(),
		"s": fmt.Sprint(seq),
	})
	if err != nil {
		panic(err)
	}
	return b
}

func txs(trades ...Trade) json.RawMessage {
	list := make([]map[string]any, 0, len(trades))
	for i, tr := range trades {
		// Distinct ids by default. They used to all be 1, which no test
		// noticed until the fill simulator started cursoring on them.
		if tr.ID == 0 {
			tr.ID = int64(i + 1)
		}
		list = append(list, map[string]any{
			"transaction_id": tr.ID,
			"side":           tr.Side,
			"price":          fmt.Sprintf("%g", tr.Price),
			"amount":         fmt.Sprintf("%g", tr.Amount),
			"executed_at":    tr.At.UnixMilli(),
		})
	}
	b, err := json.Marshal(map[string]any{"transactions": list})
	if err != nil {
		panic(err)
	}
	return b
}

func ticker(last float64, ts time.Time) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"last": fmt.Sprintf("%g", last), "buy": "1", "sell": "2",
		"timestamp": ts.UnixMilli(),
	})
	if err != nil {
		panic(err)
	}
	return b
}

// --- depth sequencing ------------------------------------------------------

// A book assembled from diffs alone is missing every level that did not happen
// to change, so it must not be served at all until a whole seeds it.
func TestBookIsNotServedBeforeTheFirstWhole(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	if err := b.ApplyDepthDiff(diff(10, t0, [][2]string{{"100", "5"}}, [][2]string{{"101", "5"}})); err != nil {
		t.Fatal(err)
	}

	s := b.Snapshot(t0)
	if s.BookSynced {
		t.Error("BookSynced is true before any depth_whole arrived")
	}
	if s.BestBid != 0 || s.BestAsk != 0 || s.SpreadBps != 0 {
		t.Errorf("unsynced snapshot reported book figures: bid=%v ask=%v spread=%v", s.BestBid, s.BestAsk, s.SpreadBps)
	}
}

// bitbank's documented algorithm, using the example from its own docs:
// diff{3}, diff{5}, diff{6}, diff{8}, whole{5} => apply 6 and 8, ignore 3 and 5.
func TestBufferedDiffsReplayOnlyAboveTheSnapshotSequence(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")

	// These arrive before the whole and must be buffered, not applied.
	must(t, b.ApplyDepthDiff(diff(3, t0, [][2]string{{"100.0", "3"}}, nil)))
	must(t, b.ApplyDepthDiff(diff(5, t0, [][2]string{{"100.0", "5"}}, nil)))
	must(t, b.ApplyDepthDiff(diff(6, t0, [][2]string{{"100.0", "6"}}, nil)))
	must(t, b.ApplyDepthDiff(diff(8, t0, [][2]string{{"100.0", "8"}}, nil)))

	must(t, b.ApplyDepthWhole(whole(5, t0, [][2]string{{"100.0", "99"}}, [][2]string{{"101.0", "1"}})))

	s := b.Snapshot(t0)
	if !s.BookSynced {
		t.Fatal("book should be synced after a whole")
	}
	// 99 from the snapshot, overwritten by diff 6 then diff 8. Diffs 3 and 5
	// are already contained in the snapshot and must not be replayed.
	if got := s.BidDepth; got != 8 {
		t.Errorf("bid depth = %v, want 8 (replay of diffs 6 then 8)", got)
	}
}

func TestDiffsOlderThanTheBookAreIgnored(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyDepthWhole(whole(100, t0, [][2]string{{"100.0", "10"}}, [][2]string{{"101.0", "10"}})))
	must(t, b.ApplyDepthDiff(diff(99, t0, [][2]string{{"100.0", "1"}}, nil)))

	if got := b.Snapshot(t0).BidDepth; got != 10 {
		t.Errorf("bid depth = %v, want 10; a stale diff was applied", got)
	}
}

func TestZeroAmountRemovesALevel(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyDepthWhole(whole(1, t0, [][2]string{{"100.0", "10"}, {"99.0", "10"}}, [][2]string{{"101.0", "10"}})))
	must(t, b.ApplyDepthDiff(diff(2, t0, [][2]string{{"100.0", "0"}}, nil)))

	s := b.Snapshot(t0)
	if s.BestBid != 99.0 {
		t.Errorf("best bid = %v, want 99 after the top level was deleted", s.BestBid)
	}
	if s.BidDepth != 10 {
		t.Errorf("bid depth = %v, want 10", s.BidDepth)
	}
}

func TestAWholeReplacesRatherThanMergesTheBook(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyDepthWhole(whole(1, t0, [][2]string{{"100.0", "10"}}, [][2]string{{"101.0", "10"}})))
	// Diffs only cover ~200 levels from the best, so a later whole is the only
	// thing that can drop a level that fell out of range.
	must(t, b.ApplyDepthWhole(whole(2, t0, [][2]string{{"90.0", "1"}}, [][2]string{{"91.0", "1"}})))

	s := b.Snapshot(t0)
	if s.BestBid != 90.0 || s.BestAsk != 91.0 {
		t.Errorf("best bid/ask = %v/%v, want 90/91", s.BestBid, s.BestAsk)
	}
	if s.BidDepth != 1 {
		t.Errorf("bid depth = %v, want 1; the old book survived the replacement", s.BidDepth)
	}
}

func TestSequenceIDAcceptsStringAndNumber(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"sequenceId":"34337099791"}`, `{"sequenceId":34337099791}`} {
		var d depthWhole
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if d.SequenceID != 34337099791 {
			t.Errorf("%s: got %d", raw, d.SequenceID)
		}
	}
}

// --- book figures ----------------------------------------------------------

func TestBestAndDepthAndSpread(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	var bids, asks [][2]string
	for i := 0; i < 12; i++ { // more than depthLevels, so the cut is exercised
		bids = append(bids, [2]string{fmt.Sprintf("%.1f", 100.0-float64(i)), "1"})
		asks = append(asks, [2]string{fmt.Sprintf("%.1f", 101.0+float64(i)), "2"})
	}
	must(t, b.ApplyDepthWhole(whole(1, t0, bids, asks)))
	s := b.Snapshot(t0)

	if s.BestBid != 100.0 {
		t.Errorf("best bid = %v, want 100 (the highest bid)", s.BestBid)
	}
	if s.BestAsk != 101.0 {
		t.Errorf("best ask = %v, want 101 (the lowest ask)", s.BestAsk)
	}
	if s.BidDepth != 10 || s.AskDepth != 20 {
		t.Errorf("depth = %v/%v, want 10/20 over the top %d levels", s.BidDepth, s.AskDepth, depthLevels)
	}
	if want := 1.0 / 100.5 * 10000; math.Abs(s.SpreadBps-want) > 1e-9 {
		t.Errorf("spread = %v bps, want %v", s.SpreadBps, want)
	}
	if want := 10.0 / 30.0; math.Abs(s.DepthImbalance-want) > 1e-9 {
		t.Errorf("imbalance = %v, want %v", s.DepthImbalance, want)
	}
}

// --- bars ------------------------------------------------------------------

// The series must be continuous in wall-clock seconds even though prints are
// not: JPY alt pairs regularly go 30s with no trade.
func TestBarSeriesIsContinuousAcrossQuietSeconds(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyTransactions(txs(Trade{At: t0, Side: "buy", Price: 100, Amount: 1})))
	must(t, b.ApplyTransactions(txs(Trade{At: t0.Add(5 * time.Second), Side: "sell", Price: 110, Amount: 2})))

	s := b.Snapshot(t0.Add(5 * time.Second))
	if len(s.Bars) != 6 {
		t.Fatalf("got %d bars, want 6 (one per second from t0 to t0+5s)", len(s.Bars))
	}
	for i, bar := range s.Bars {
		wantStart := t0.Add(time.Duration(i) * time.Second)
		if !bar.Start.Equal(wantStart) {
			t.Errorf("bar %d starts at %v, want %v", i, bar.Start, wantStart)
		}
	}
	for i := 1; i < 5; i++ {
		if s.Bars[i].Traded {
			t.Errorf("bar %d is marked traded but nothing printed in that second", i)
		}
		if s.Bars[i].Close != 100 || s.Bars[i].Volume != 0 {
			t.Errorf("bar %d = close %v volume %v, want the carried close 100 and no volume", i, s.Bars[i].Close, s.Bars[i].Volume)
		}
	}
	if last := s.Bars[5]; !last.Traded || last.Close != 110 || last.Volume != 2 || last.Sells != 2 {
		t.Errorf("last bar = %+v, want a traded bar closing at 110 with 2 sold", last)
	}
}

func TestSnapshotExtendsToNowWithoutMutatingTheBook(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyTransactions(txs(Trade{At: t0, Side: "buy", Price: 100, Amount: 1})))

	if got := len(b.Snapshot(t0.Add(9 * time.Second)).Bars); got != 10 {
		t.Fatalf("snapshot bars = %d, want 10", got)
	}
	// Taking a snapshot must not advance the stored series; a later snapshot
	// for an earlier instant would otherwise see bars from the future.
	if got := len(b.bars); got != 1 {
		t.Fatalf("stored bars = %d, want 1; Snapshot mutated the book", got)
	}
	if got := len(b.Snapshot(t0.Add(2 * time.Second)).Bars); got != 3 {
		t.Fatalf("second snapshot bars = %d, want 3", got)
	}
}

func TestTickerKeepsTheSeriesAliveOnAPairWithNoPrints(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyTicker(ticker(202.25, t0)))

	s := b.Snapshot(t0)
	if s.Last != 202.25 {
		t.Errorf("last = %v, want 202.25 seeded from the ticker", s.Last)
	}
	if s.LastTradeAgo != 0 {
		t.Errorf("LastTradeAgo = %v, want 0: the ticker is not a print", s.LastTradeAgo)
	}
	if s.Trades30s != 0 {
		t.Errorf("Trades30s = %d, want 0", s.Trades30s)
	}
}

func TestOutOfOrderPrintsFoldIntoTheRightSecond(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	// One frame, newest first — bitbank batches prints and does not promise order.
	must(t, b.ApplyTransactions(txs(
		Trade{At: t0.Add(2 * time.Second), Side: "buy", Price: 110, Amount: 1},
		Trade{At: t0, Side: "sell", Price: 100, Amount: 3},
	)))

	s := b.Snapshot(t0.Add(2 * time.Second))
	if len(s.Bars) != 3 {
		t.Fatalf("got %d bars, want 3", len(s.Bars))
	}
	if s.Bars[0].Volume != 3 || !s.Bars[0].Traded {
		t.Errorf("first bar = %+v, want the 3-unit print folded into it", s.Bars[0])
	}
	if s.Last != 110 {
		t.Errorf("last = %v, want 110: the newest print wins regardless of frame order", s.Last)
	}
}

func TestBarCapacityIsBoundedAcrossALongGap(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyTransactions(txs(Trade{At: t0, Side: "buy", Price: 100, Amount: 1})))
	// An hour of silence must not allocate 3,600 bars.
	must(t, b.ApplyTicker(ticker(100, t0.Add(time.Hour))))

	if got := len(b.bars); got > barCapacity {
		t.Fatalf("stored bars = %d, want at most %d", got, barCapacity)
	}
}

// --- indicators ------------------------------------------------------------

func TestReturnsAreOverSecondsNotOverPrints(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyTransactions(txs(Trade{At: t0, Side: "buy", Price: 100, Amount: 1})))
	// Exactly 60 seconds later, one print 1% higher. Only two prints exist, so
	// a print-indexed series would report a 0% 60s return.
	must(t, b.ApplyTransactions(txs(Trade{At: t0.Add(60 * time.Second), Side: "buy", Price: 101, Amount: 1})))

	s := b.Snapshot(t0.Add(60 * time.Second))
	if want := 0.01; math.Abs(s.Ret60s-want) > 1e-9 {
		t.Errorf("Ret60s = %v, want %v", s.Ret60s, want)
	}
}

func TestIndicatorMaths(t *testing.T) {
	t.Parallel()
	closes := []float64{1, 2, 3, 4}

	if got := mean(closes); got != 2.5 {
		t.Errorf("mean = %v, want 2.5", got)
	}
	if got := mean(nil); got != 0 {
		t.Errorf("mean of nothing = %v, want 0", got)
	}
	if got := ret(closes, 3); got != 3.0 {
		t.Errorf("ret over 3 = %v, want 3", got)
	}
	if got := ret(closes, 10); got != 0 {
		t.Errorf("ret over a window longer than the series = %v, want 0", got)
	}
	if got := len(tail(closes, 2)); got != 2 {
		t.Errorf("tail(2) length = %d, want 2", got)
	}

	// A series that changes by a constant log step has zero dispersion.
	flat := []float64{100, 110, 121, 133.1}
	if got := stdevLogRet(flat); math.Abs(got) > 1e-9 {
		t.Errorf("stdev of a constant-growth series = %v, want 0", got)
	}
	if got := stdevLogRet([]float64{100}); got != 0 {
		t.Errorf("stdev of a single point = %v, want 0", got)
	}
}

func TestBuyRatioAndTradeCountUseThe30sWindow(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyTransactions(txs(
		Trade{At: t0.Add(-45 * time.Second), Side: "buy", Price: 100, Amount: 100}, // outside the window
		Trade{At: t0.Add(-10 * time.Second), Side: "buy", Price: 100, Amount: 3},
		Trade{At: t0.Add(-5 * time.Second), Side: "sell", Price: 100, Amount: 1},
	)))

	s := b.Snapshot(t0)
	if s.Trades30s != 2 {
		t.Errorf("Trades30s = %d, want 2", s.Trades30s)
	}
	if want := 0.75; math.Abs(s.BuyRatio30s-want) > 1e-9 {
		t.Errorf("BuyRatio30s = %v, want %v", s.BuyRatio30s, want)
	}
}

// --- health flags ----------------------------------------------------------

func TestStaleAndHalted(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	if !b.Snapshot(t0).Stale {
		t.Error("a book that has received nothing should be stale")
	}

	must(t, b.ApplyCircuitBreak(json.RawMessage(`{"mode":"NONE","fee_type":"NORMAL"}`)))
	if s := b.Snapshot(time.Now().UTC()); s.Stale {
		t.Error("a book that just received data should not be stale")
	} else if s.Halted() {
		t.Error("mode NONE is normal trading, not a halt")
	}

	must(t, b.ApplyCircuitBreak(json.RawMessage(`{"mode":"CIRCUIT_BREAK","fee_type":"SELL_MAKER"}`)))
	if s := b.Snapshot(time.Now().UTC()); !s.Halted() {
		t.Error("mode CIRCUIT_BREAK should report halted")
	}
}

func TestMalformedPayloadsAreRejectedNotAbsorbed(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	for name, raw := range map[string]json.RawMessage{
		"whole": json.RawMessage(`{"bids":"not-an-array"}`),
		"diff":  json.RawMessage(`{"b":5}`),
		"tx":    json.RawMessage(`[]`),
	} {
		var err error
		switch name {
		case "whole":
			err = b.ApplyDepthWhole(raw)
		case "diff":
			err = b.ApplyDepthDiff(raw)
		case "tx":
			err = b.ApplyTransactions(raw)
		}
		if err == nil {
			t.Errorf("%s: malformed payload was accepted", name)
		}
	}
	if b.Snapshot(t0).BookSynced {
		t.Error("a malformed whole must not mark the book synced")
	}
}

func TestConcurrentIngestAndSnapshot(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			at := t0.Add(time.Duration(i) * time.Second)
			_ = b.ApplyDepthWhole(whole(uint64(i+1), at, [][2]string{{"100", "1"}}, [][2]string{{"101", "1"}}))
			_ = b.ApplyTransactions(txs(Trade{At: at, Side: "buy", Price: 100, Amount: 1}))
			_ = b.ApplyTicker(ticker(100, at))
		}
	}()
	for i := 0; i < 200; i++ {
		_ = b.Snapshot(t0.Add(time.Duration(i) * time.Second))
	}
	<-done
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Every window the state text quotes must actually be computable from a series
// the book is willing to hold.
//
// It was not. barCapacity was 300 while a 300-second return needs 301 samples,
// so "Return 300s" read +0.000% in every state ever rendered — not during
// warmup, but permanently. An hour of live output made it obvious; nothing in
// the unit tests did, because they all built series shorter than the cap.
func TestEveryQuotedWindowIsComputable(t *testing.T) {
	t.Parallel()
	b := NewBook("xrp_jpy")
	must(t, b.ApplyDepthWhole(whole(1, t0, [][2]string{{"100", "1"}}, [][2]string{{"101", "1"}})))

	// Ten minutes of a steadily moving price — twice the longest window, so
	// nothing here is a warmup effect.
	const span = 600
	for i := 0; i < span; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		must(t, b.ApplyTransactions(txs(Trade{At: at, Side: "buy", Price: 100 + float64(i)*0.01, Amount: 1})))
	}
	s := b.Snapshot(t0.Add((span - 1) * time.Second))

	if got := s.HistorySeconds(); got <= longestWindowSec {
		t.Fatalf("series holds %ds, which cannot express a %ds window", got, longestWindowSec)
	}
	for _, c := range []struct {
		name string
		got  float64
	}{
		{"Ret60s", s.Ret60s},
		{"Ret300s", s.Ret300s},
		{"SMA20", s.SMA20},
		{"SMA60", s.SMA60},
		{"VolBps", s.VolBps},
		{"High5m", s.High5m},
		{"Low5m", s.Low5m},
	} {
		if c.got == 0 {
			t.Errorf("%s is zero on a ten-minute rising series; the window cannot be computed", c.name)
		}
	}
	if s.High5m == s.Low5m {
		t.Error("the 5m range is empty on a steadily rising series")
	}

	// And the rendered text must not be claiming a flat market.
	text := Render(s, Position{})
	for _, forbidden := range []string{"Return 300s: +0.000%", "Return 60s: +0.000%"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("state text still reports %q after ten minutes of movement", forbidden)
		}
	}
}
