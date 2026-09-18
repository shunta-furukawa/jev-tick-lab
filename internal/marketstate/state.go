// Package marketstate keeps the live view of the market in memory.
//
// Design rule: every number a decision might need is computed HERE, in ordinary
// deterministic Go. The model is never asked to calculate something code can
// compute exactly — that is both TypeSafe's own guidance and the only way the
// numbers stay testable.
//
// The payload shapes below were verified against a live connection on
// 2026-09-17; see docs/stream-verification.md for the captured frames.
package marketstate

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	barCapacity   = 300 // 5 minutes of 1s bars
	tradeCapacity = 2000
	depthLevels   = 10 // levels aggregated for the imbalance figures

	// A depth_diff that arrives before the first depth_whole cannot be applied
	// yet. bitbank sends a whole every few seconds, so this buffer only has to
	// cover the gap at connect time.
	pendingDiffCapacity = 512

	// No market data at all for this long means the feed is broken, which is a
	// different condition from a pair that is merely quiet.
	feedStaleAfter = 10 * time.Second
)

// Bar is a one-second OHLCV candle.
//
// The series is CONTINUOUS: a second in which nothing traded still gets a bar,
// carrying the previous close with zero volume. This matters more than it
// looks. bitbank JPY alt pairs regularly go 30s or more without a print (see
// docs/stream-verification.md), so a series built only from trades would make
// "the last 60 bars" span several minutes while the state text called it 60
// seconds — every time-labelled number downstream would be a lie.
type Bar struct {
	Start  time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
	Buys   float64 // taker-buy volume
	Sells  float64 // taker-sell volume
	Traded bool    // false for a carried-forward (no-print) second
}

// Trade is a single execution from the transactions room.
type Trade struct {
	At     time.Time
	Side   string // "buy" | "sell" (taker side)
	Price  float64
	Amount float64
}

// Snapshot is an immutable copy handed to the renderer and the decision layer.
type Snapshot struct {
	At        time.Time
	Pair      string
	Last      float64
	BestBid   float64
	BestAsk   float64
	Mid       float64
	SpreadBps float64

	BidDepth       float64 // sum of amounts over depthLevels
	AskDepth       float64
	DepthImbalance float64 // bid/(bid+ask), 0.5 == balanced

	// Bars are excluded from the JSONL record on purpose: 300 bars per record
	// at one record per second is ~1GB/day of data that StateText already
	// carries in the form the model actually saw.
	Bars []Bar `json:"-"`

	Ret60s  float64 // fractional return over the last 60s
	Ret300s float64
	SMA20   float64
	SMA60   float64
	VolBps  float64 // stdev of 1s log returns over 60s, in bps
	High5m  float64
	Low5m   float64

	BuyRatio30s float64 // taker-buy share of volume over the last 30s
	Trades30s   int     // number of prints in the last 30s

	// Feed health. A decision taken on a book that was never seeded, or during
	// an exchange halt, is worse than no decision at all.
	BookSynced   bool
	BookAgeMs    float64   // age of the last depth update, exchange clock
	LastTradeAt  time.Time // zero when no print has been seen at all, which is
	LastTradeAgo float64   // distinct from having just traded (LastTradeAgo 0) seen

	CircuitBreak string // bitbank circuit_break_info mode; "NONE" when trading normally
	FeeType      string // NORMAL | SELL_MAKER | BUY_MAKER | DYNAMIC

	Stale bool // true when no market data of any kind has arrived recently
}

// Halted reports whether the exchange itself says this market is not trading
// normally. This is a fact off the wire, not a judgement — the model is never
// asked about it.
func (s Snapshot) Halted() bool {
	return s.CircuitBreak != "" && s.CircuitBreak != "NONE"
}

// Book is the concurrency-safe live state.
type Book struct {
	mu   sync.RWMutex
	pair string

	bids map[float64]float64
	asks map[float64]float64

	// Depth sequencing, per bitbank's "How to manage a local order book
	// correctly". Sequence ids rise monotonically but are NOT consecutive, so
	// a gap is not detectable by arithmetic — correctness comes from replaying
	// buffered diffs against each whole instead.
	synced  bool
	seq     uint64
	pending []pendingDiff
	bookAt  time.Time

	bars   []Bar
	trades []Trade

	lastPrice   float64
	lastTradeAt time.Time
	lastFeedAt  time.Time

	circuitBreak string
	feeType      string
}

type pendingDiff struct {
	seq  uint64
	asks [][2]string
	bids [][2]string
	at   time.Time
}

func NewBook(pair string) *Book {
	return &Book{
		pair: pair,
		bids: make(map[float64]float64),
		asks: make(map[float64]float64),
	}
}

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

// seqID decodes bitbank's sequence id. depth_diff sends it as a JSON string
// ("34337088955") and the docs table calls depth_whole's a number, so accept
// both rather than trusting either.
type seqID uint64

func (s *seqID) UnmarshalJSON(b []byte) error {
	text := string(b)
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		text = text[1 : len(text)-1]
	}
	if text == "" || text == "null" {
		*s = 0
		return nil
	}
	v, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return err
	}
	*s = seqID(v)
	return nil
}

// --- ingestion -------------------------------------------------------------

type tickerPayload struct {
	Sell      string `json:"sell"`
	Buy       string `json:"buy"`
	Last      string `json:"last"`
	Timestamp int64  `json:"timestamp"`
}

// ApplyTicker records the last traded price.
//
// The ticker room is the only one that reports a price on a quiet pair, so it
// is what keeps the bar series alive between prints. It does not touch volume:
// a carried price is not a trade.
func (b *Book) ApplyTicker(raw json.RawMessage) error {
	var t tickerPayload
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	price := f(t.Last)
	if price <= 0 {
		return nil
	}
	at := time.UnixMilli(t.Timestamp).UTC()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastFeedAt = time.Now().UTC()
	b.lastPrice = price
	b.sealTo(at.Truncate(time.Second), price)
	return nil
}

type depthWhole struct {
	Asks       [][2]string `json:"asks"`
	Bids       [][2]string `json:"bids"`
	Timestamp  int64       `json:"timestamp"`
	SequenceID seqID       `json:"sequenceId"`
}

// ApplyDepthWhole replaces the book with a full snapshot, then replays the
// buffered diffs that are newer than it. bitbank sends wholes delayed relative
// to diffs, so without the replay the book loses every update in that window.
func (b *Book) ApplyDepthWhole(raw json.RawMessage) error {
	var d depthWhole
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.bids = make(map[float64]float64, len(d.Bids))
	b.asks = make(map[float64]float64, len(d.Asks))
	for _, lv := range d.Bids {
		b.bids[f(lv[0])] = f(lv[1])
	}
	for _, lv := range d.Asks {
		b.asks[f(lv[0])] = f(lv[1])
	}
	b.seq = uint64(d.SequenceID)
	b.synced = true
	b.bookAt = time.UnixMilli(d.Timestamp).UTC()
	b.lastFeedAt = time.Now().UTC()

	// Replay in ascending sequence order; anything at or below the snapshot is
	// already contained in it and is dropped.
	sort.Slice(b.pending, func(i, j int) bool { return b.pending[i].seq < b.pending[j].seq })
	for _, p := range b.pending {
		if p.seq <= b.seq {
			continue
		}
		b.applyLevels(p.asks, p.bids)
		b.seq = p.seq
		if p.at.After(b.bookAt) {
			b.bookAt = p.at
		}
	}
	b.pending = b.pending[:0]
	return nil
}

type depthDiff struct {
	Asks       [][2]string `json:"a"`
	Bids       [][2]string `json:"b"`
	Timestamp  int64       `json:"t"`
	SequenceID seqID       `json:"s"`
}

// ApplyDepthDiff applies an incremental update. An amount of zero removes the
// level. Diffs that arrive before the first whole are buffered, not applied:
// a book assembled from diffs alone is missing every level that did not happen
// to change, and would report a confidently wrong best bid and ask.
func (b *Book) ApplyDepthDiff(raw json.RawMessage) error {
	var d depthDiff
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	seq := uint64(d.SequenceID)
	at := time.UnixMilli(d.Timestamp).UTC()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastFeedAt = time.Now().UTC()

	if !b.synced {
		if len(b.pending) >= pendingDiffCapacity {
			b.pending = b.pending[1:]
		}
		b.pending = append(b.pending, pendingDiff{seq: seq, asks: d.Asks, bids: d.Bids, at: at})
		return nil
	}
	if seq <= b.seq {
		return nil // already contained in the book
	}
	b.applyLevels(d.Asks, d.Bids)
	b.seq = seq
	b.bookAt = at
	return nil
}

// applyLevels writes absolute amounts into the book. Caller holds the lock.
func (b *Book) applyLevels(asks, bids [][2]string) {
	apply := func(side map[float64]float64, levels [][2]string) {
		for _, lv := range levels {
			price, amount := f(lv[0]), f(lv[1])
			if amount == 0 {
				delete(side, price)
			} else {
				side[price] = amount
			}
		}
	}
	apply(b.asks, asks)
	apply(b.bids, bids)
}

type circuitBreakPayload struct {
	Mode    string `json:"mode"`
	FeeType string `json:"fee_type"`
}

// ApplyCircuitBreak records the exchange's own trading-mode flag. Anything
// other than NONE means the market is in an auction or halted, which is a hard
// brake in decide — not something to ask the model about.
func (b *Book) ApplyCircuitBreak(raw json.RawMessage) error {
	var c circuitBreakPayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.circuitBreak, b.feeType = c.Mode, c.FeeType
	b.lastFeedAt = time.Now().UTC()
	return nil
}

type transactions struct {
	Transactions []struct {
		TransactionID int64  `json:"transaction_id"`
		Side          string `json:"side"`
		Price         string `json:"price"`
		Amount        string `json:"amount"`
		ExecutedAt    int64  `json:"executed_at"` // epoch millis
	} `json:"transactions"`
}

// ApplyTransactions folds trade prints into the trade buffer and 1s bars.
func (b *Book) ApplyTransactions(raw json.RawMessage) error {
	var t transactions
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	// bitbank batches prints and sends them newest first, so a frame can walk
	// backwards across a second boundary. Fold them in time order: the first
	// print in a frame is what seeds the bar series, and seeding it from the
	// newest print would drop everything older in the same frame.
	sort.SliceStable(t.Transactions, func(i, j int) bool {
		return t.Transactions[i].ExecutedAt < t.Transactions[j].ExecutedAt
	})

	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastFeedAt = time.Now().UTC()

	for _, x := range t.Transactions {
		tr := Trade{
			At:     time.UnixMilli(x.ExecutedAt).UTC(),
			Side:   x.Side,
			Price:  f(x.Price),
			Amount: f(x.Amount),
		}
		if tr.Price <= 0 {
			continue
		}
		b.trades = append(b.trades, tr)
		if !tr.At.Before(b.lastTradeAt) {
			b.lastPrice = tr.Price
			b.lastTradeAt = tr.At
		}
		b.foldIntoBar(tr)
	}
	if n := len(b.trades); n > tradeCapacity {
		b.trades = append([]Trade(nil), b.trades[n-tradeCapacity:]...)
	}
	return nil
}

// sealTo makes the bar series continuous through sec, seeding it at price if
// it is empty. Caller holds the write lock.
func (b *Book) sealTo(sec time.Time, price float64) {
	b.bars = extend(b.bars, sec, price)
}

// extend flat-fills the series up to sec. A filled bar carries the previous
// close and zero volume, and is marked Traded=false.
func extend(bars []Bar, sec time.Time, price float64) []Bar {
	sec = sec.Truncate(time.Second)
	if len(bars) == 0 {
		if price <= 0 {
			return bars
		}
		return []Bar{{Start: sec, Open: price, High: price, Low: price, Close: price}}
	}
	last := bars[len(bars)-1]
	if !sec.After(last.Start) {
		return bars
	}
	// A long outage would otherwise allocate one bar per missed second.
	if gap := int(sec.Sub(last.Start) / time.Second); gap > barCapacity {
		last.Start = sec.Add(-time.Duration(barCapacity) * time.Second)
		bars = []Bar{last}
	}
	for next := bars[len(bars)-1].Start.Add(time.Second); !next.After(sec); next = next.Add(time.Second) {
		c := bars[len(bars)-1].Close
		bars = append(bars, Bar{Start: next, Open: c, High: c, Low: c, Close: c})
	}
	if n := len(bars); n > barCapacity {
		bars = append([]Bar(nil), bars[n-barCapacity:]...)
	}
	return bars
}

func (b *Book) foldIntoBar(tr Trade) {
	sec := tr.At.Truncate(time.Second)

	if len(b.bars) == 0 {
		b.bars = []Bar{{Start: sec, Open: tr.Price, High: tr.Price, Low: tr.Price, Close: tr.Price}}
	} else if sec.After(b.bars[len(b.bars)-1].Start) {
		b.sealTo(sec, tr.Price)
	}

	// Prints inside one frame can be out of order and can land on an earlier
	// second than the series head; fold them where they belong, or drop them
	// if they predate the window entirely.
	idx := -1
	for i := len(b.bars) - 1; i >= 0; i-- {
		if b.bars[i].Start.Equal(sec) {
			idx = i
			break
		}
		if b.bars[i].Start.Before(sec) {
			break
		}
	}
	if idx < 0 {
		return
	}

	bar := &b.bars[idx]
	if !bar.Traded {
		// A carried-forward second becomes a real one on its first print.
		bar.Open, bar.High, bar.Low = tr.Price, tr.Price, tr.Price
		bar.Traded = true
	}
	bar.High = math.Max(bar.High, tr.Price)
	bar.Low = math.Min(bar.Low, tr.Price)
	bar.Close = tr.Price
	bar.Volume += tr.Amount
	if tr.Side == "buy" {
		bar.Buys += tr.Amount
	} else {
		bar.Sells += tr.Amount
	}
}

// --- derivation ------------------------------------------------------------

// Snapshot computes every derived figure at once, under a single read lock.
func (b *Book) Snapshot(now time.Time) Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	now = now.UTC()
	s := Snapshot{
		At:           now,
		Pair:         b.pair,
		Last:         b.lastPrice,
		BookSynced:   b.synced,
		CircuitBreak: b.circuitBreak,
		FeeType:      b.feeType,
	}
	s.Stale = b.lastFeedAt.IsZero() || now.Sub(b.lastFeedAt) > feedStaleAfter
	if !b.lastTradeAt.IsZero() {
		s.LastTradeAt = b.lastTradeAt
		s.LastTradeAgo = now.Sub(b.lastTradeAt).Seconds()
	}
	if !b.bookAt.IsZero() {
		s.BookAgeMs = float64(now.Sub(b.bookAt).Milliseconds())
	}

	if b.synced {
		s.BestBid, s.BidDepth = bestAndDepth(b.bids, true)
		s.BestAsk, s.AskDepth = bestAndDepth(b.asks, false)
		if s.BestBid > 0 && s.BestAsk > 0 {
			s.Mid = (s.BestBid + s.BestAsk) / 2
			s.SpreadBps = (s.BestAsk - s.BestBid) / s.Mid * 10000
		}
		if total := s.BidDepth + s.AskDepth; total > 0 {
			s.DepthImbalance = s.BidDepth / total
		}
	}

	// Extend a copy rather than the stored series: Snapshot holds a read lock
	// and must not mutate. The result is the same bars the next ingest would
	// produce, so a re-render of a logged snapshot matches the live one.
	s.Bars = extend(append([]Bar(nil), b.bars...), now, b.lastPrice)
	closes := closesOf(s.Bars)

	s.SMA20 = mean(tail(closes, 20))
	s.SMA60 = mean(tail(closes, 60))
	s.Ret60s = ret(closes, 60)
	s.Ret300s = ret(closes, 300)
	s.VolBps = stdevLogRet(tail(closes, 60)) * 10000
	s.High5m, s.Low5m = highLow(s.Bars)

	var buys, sells float64
	cutoff := now.Add(-30 * time.Second)
	for i := len(b.trades) - 1; i >= 0; i-- {
		if b.trades[i].At.Before(cutoff) {
			break
		}
		s.Trades30s++
		if b.trades[i].Side == "buy" {
			buys += b.trades[i].Amount
		} else {
			sells += b.trades[i].Amount
		}
	}
	if buys+sells > 0 {
		s.BuyRatio30s = buys / (buys + sells)
	}
	return s
}

func bestAndDepth(side map[float64]float64, isBid bool) (best, depth float64) {
	prices := make([]float64, 0, len(side))
	for p := range side {
		prices = append(prices, p)
	}
	if len(prices) == 0 {
		return 0, 0
	}
	sort.Float64s(prices)
	if isBid {
		// Highest bids first.
		for i, j := 0, len(prices)-1; i < j; i, j = i+1, j-1 {
			prices[i], prices[j] = prices[j], prices[i]
		}
	}
	best = prices[0]
	for i := 0; i < len(prices) && i < depthLevels; i++ {
		depth += side[prices[i]]
	}
	return best, depth
}

func closesOf(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

func tail(xs []float64, n int) []float64 {
	if len(xs) <= n {
		return xs
	}
	return xs[len(xs)-n:]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func ret(closes []float64, n int) float64 {
	if len(closes) <= n || closes[len(closes)-1-n] == 0 {
		return 0
	}
	from, to := closes[len(closes)-1-n], closes[len(closes)-1]
	return (to - from) / from
}

func stdevLogRet(closes []float64) float64 {
	if len(closes) < 3 {
		return 0
	}
	rets := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		if closes[i-1] > 0 && closes[i] > 0 {
			rets = append(rets, math.Log(closes[i]/closes[i-1]))
		}
	}
	if len(rets) < 2 {
		return 0
	}
	m := mean(rets)
	var acc float64
	for _, r := range rets {
		acc += (r - m) * (r - m)
	}
	return math.Sqrt(acc / float64(len(rets)))
}

func highLow(bars []Bar) (high, low float64) {
	if len(bars) == 0 {
		return 0, 0
	}
	high, low = bars[0].High, bars[0].Low
	for _, b := range bars {
		high = math.Max(high, b.High)
		low = math.Min(low, b.Low)
	}
	return high, low
}
