// Package marketstate keeps the live view of the market in memory.
//
// Design rule: every number a decision might need is computed HERE, in ordinary
// deterministic Go. The model is never asked to calculate something code can
// compute exactly — that is both TypeSafe's own guidance and the only way the
// numbers stay testable.
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
)

// Bar is a one-second OHLCV candle built from trade prints.
type Bar struct {
	Start  time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
	Buys   float64 // taker-buy volume
	Sells  float64 // taker-sell volume
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
	SpreadBps float64

	BidDepth float64 // sum of amounts over depthLevels
	AskDepth float64
	DepthImbalance float64 // bid/(bid+ask), 0.5 == balanced

	Bars []Bar // oldest first

	Ret60s  float64 // fractional return over the last 60s
	Ret300s float64
	SMA20   float64
	SMA60   float64
	VolBps  float64 // stdev of 1s log returns over 60s, in bps
	High5m  float64
	Low5m   float64

	BuyRatio30s float64 // taker-buy share of volume over the last 30s

	Stale bool // true when no trade has printed recently
}

// Book is the concurrency-safe live state.
type Book struct {
	mu   sync.RWMutex
	pair string

	bids map[float64]float64
	asks map[float64]float64
	seq  string

	bars   []Bar
	trades []Trade

	lastPrice  float64
	lastUpdate time.Time
}

func NewBook(pair string) *Book {
	return &Book{
		pair: pair,
		bids: make(map[float64]float64),
		asks: make(map[float64]float64),
	}
}

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

// --- ingestion -------------------------------------------------------------

type depthWhole struct {
	Asks       [][2]string `json:"asks"`
	Bids       [][2]string `json:"bids"`
	SequenceID string      `json:"sequenceId"`
}

// ApplyDepthWhole replaces the book with a full snapshot.
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
	b.seq = d.SequenceID
	return nil
}

type depthDiff struct {
	Asks       [][2]string `json:"a"`
	Bids       [][2]string `json:"b"`
	SequenceID string      `json:"s"`
}

// ApplyDepthDiff applies an incremental update. An amount of zero removes the level.
func (b *Book) ApplyDepthDiff(raw json.RawMessage) error {
	var d depthDiff
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

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
	apply(b.bids, d.Bids)
	apply(b.asks, d.Asks)
	b.seq = d.SequenceID
	return nil
}

type transactions struct {
	Transactions []struct {
		Side       string `json:"side"`
		Price      string `json:"price"`
		Amount     string `json:"amount"`
		ExecutedAt int64  `json:"executed_at"` // epoch millis
	} `json:"transactions"`
}

// ApplyTransactions folds trade prints into the trade buffer and 1s bars.
func (b *Book) ApplyTransactions(raw json.RawMessage) error {
	var t transactions
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, x := range t.Transactions {
		tr := Trade{
			At:     time.UnixMilli(x.ExecutedAt),
			Side:   x.Side,
			Price:  f(x.Price),
			Amount: f(x.Amount),
		}
		b.trades = append(b.trades, tr)
		b.lastPrice = tr.Price
		b.lastUpdate = time.Now()
		b.foldIntoBar(tr)
	}
	if n := len(b.trades); n > tradeCapacity {
		b.trades = append([]Trade(nil), b.trades[n-tradeCapacity:]...)
	}
	return nil
}

func (b *Book) foldIntoBar(tr Trade) {
	sec := tr.At.Truncate(time.Second)

	if n := len(b.bars); n > 0 && b.bars[n-1].Start.Equal(sec) {
		bar := &b.bars[n-1]
		bar.High = math.Max(bar.High, tr.Price)
		bar.Low = math.Min(bar.Low, tr.Price)
		bar.Close = tr.Price
		bar.Volume += tr.Amount
		if tr.Side == "buy" {
			bar.Buys += tr.Amount
		} else {
			bar.Sells += tr.Amount
		}
		return
	}

	bar := Bar{Start: sec, Open: tr.Price, High: tr.Price, Low: tr.Price, Close: tr.Price, Volume: tr.Amount}
	if tr.Side == "buy" {
		bar.Buys = tr.Amount
	} else {
		bar.Sells = tr.Amount
	}
	b.bars = append(b.bars, bar)

	if n := len(b.bars); n > barCapacity {
		b.bars = append([]Bar(nil), b.bars[n-barCapacity:]...)
	}
}

// --- derivation ------------------------------------------------------------

// Snapshot computes every derived figure at once, under a single read lock.
func (b *Book) Snapshot(now time.Time) Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	s := Snapshot{At: now, Pair: b.pair, Last: b.lastPrice}
	s.Stale = b.lastUpdate.IsZero() || now.Sub(b.lastUpdate) > 10*time.Second

	s.BestBid, s.BidDepth = bestAndDepth(b.bids, true)
	s.BestAsk, s.AskDepth = bestAndDepth(b.asks, false)
	if s.BestBid > 0 && s.BestAsk > 0 {
		mid := (s.BestBid + s.BestAsk) / 2
		s.SpreadBps = (s.BestAsk - s.BestBid) / mid * 10000
	}
	if total := s.BidDepth + s.AskDepth; total > 0 {
		s.DepthImbalance = s.BidDepth / total
	}

	s.Bars = append([]Bar(nil), b.bars...)
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
