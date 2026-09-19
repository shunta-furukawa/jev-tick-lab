package marketstate

import "sort"

// Level is one price level of the resting book.
type Level struct {
	Price  float64
	Amount float64
}

// Ladder returns the top n levels of each side, best first.
//
// Snapshot deliberately carries only aggregates — it is written to every tick
// record, and a full ladder per second is gigabytes a day of something the
// state text already summarises. The fill simulator needs the levels
// themselves: a taker order eats through them one at a time, and the price it
// gets is the volume-weighted average of what it consumed, not the touch.
//
// Returns synced=false when no depth_whole has seeded the book. A simulator
// must not fill against a book that was never seeded, for the same reason
// decide.Compose gates on it: the levels would be an artefact of whichever
// diffs happened to arrive, not the market.
func (b *Book) Ladder(n int) (bids, asks []Level, synced bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.synced {
		return nil, nil, false
	}
	return side(b.bids, n, true), side(b.asks, n, false), true
}

func side(m map[float64]float64, n int, isBid bool) []Level {
	prices := make([]float64, 0, len(m))
	for p, amt := range m {
		if amt > 0 {
			prices = append(prices, p)
		}
	}
	sort.Float64s(prices)
	if isBid {
		for i, j := 0, len(prices)-1; i < j; i, j = i+1, j-1 {
			prices[i], prices[j] = prices[j], prices[i]
		}
	}
	if n > 0 && len(prices) > n {
		prices = prices[:n]
	}
	out := make([]Level, 0, len(prices))
	for _, p := range prices {
		out = append(out, Level{Price: p, Amount: m[p]})
	}
	return out
}

// TradesAfter returns the prints with an id above cursor, oldest first, along
// with the new cursor to pass next time.
//
// Prints are what move a resting order up its queue. A maker fill cannot be
// inferred from the book alone: a price touching a limit says nothing about
// whether the volume ahead of it was consumed. Only prints do, so the
// simulator must see every one of them exactly once — dropping prints
// under-fills a passive order, replaying them over-fills it.
//
// The cursor is the transaction id rather than a timestamp because bitbank
// timestamps are milliseconds and several prints routinely share one.
//
// It scans the whole buffer rather than binary-searching it: bitbank batches
// prints and can send a frame carrying ids older than the previous frame's, so
// the buffer is very nearly sorted but not guaranteed to be, and "very nearly"
// is not something a fill model should be built on. The buffer is 2,000 deep
// and this runs once a tick.
func (b *Book) TradesAfter(cursor int64) (out []Trade, next int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	next = cursor
	for _, tr := range b.trades {
		if tr.ID <= cursor {
			continue
		}
		out = append(out, tr)
		if tr.ID > next {
			next = tr.ID
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, next
}

// LatestTradeID is the cursor a fresh consumer should start from, so that it
// sees what happens next rather than replaying the buffer.
func (b *Book) LatestTradeID() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var max int64
	for _, tr := range b.trades {
		if tr.ID > max {
			max = tr.ID
		}
	}
	return max
}
