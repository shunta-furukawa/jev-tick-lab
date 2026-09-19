// Package risk is the set of brakes that stand between a bug and the balance.
//
// Everything else in this repository can be wrong and cost a wrong number in a
// log file. Once -mode live is running, a bug spends money, and it spends it
// at one decision per second without getting tired. These are the limits that
// make that bounded.
//
// It is pure and has no I/O, like internal/decide, so every rule here is
// testable without a network or an exchange. cmd/bot supplies the clock and
// the facts.
package risk

import (
	"fmt"
	"time"
)

// Verdict is what the risk layer allows right now.
type Verdict string

const (
	// Trade: entries and exits both permitted.
	Trade Verdict = "trade"
	// ExitOnly: close what is held, open nothing new. This is the important
	// middle state — a brake that traps a position is not a brake.
	ExitOnly Verdict = "exit_only"
	// Halt: place nothing at all. Reserved for states where an order would be
	// acting on something we cannot see.
	Halt Verdict = "halt"
)

// Limits are the configured bounds. All money is in JPY.
type Limits struct {
	// MaxDailyLossJPY stops new entries once the day's realised loss reaches
	// it. Counted from realised round trips, because unrealised P&L on an open
	// position is not a loss until it is taken and would otherwise flap the
	// brake on and off with the price.
	MaxDailyLossJPY float64

	// MaxNotionalJPY is the size of one entry.
	MaxNotionalJPY float64

	// MaxOpenNotionalJPY caps total exposure. With no pyramiding this equals
	// one entry, but it is enforced separately so that a pyramiding bug cannot
	// quietly become an unbounded position.
	MaxOpenNotionalJPY float64

	// MaxTradesPerDay bounds the fee bleed. The per-order size bounds nothing
	// on its own: the same 3,000 JPY recycled 500 times pays 500 round trips
	// of fees. This is the limit that actually caps that.
	MaxTradesPerDay int

	// MaxOrdersPerMinute is the runaway brake. A loop that submits and
	// immediately cancels does not move the position and so trips none of the
	// limits above, while still paying for every crossing it makes.
	MaxOrdersPerMinute int

	// StaleAfter is how long without market data before the feed counts as
	// lost. Acting on a stale book is acting on a market that has moved.
	StaleAfter time.Duration

	// FlattenOnStale closes an open position when the feed is lost, rather
	// than holding it blind. On a laptop this is the one that matters: a
	// closed lid is an open position nobody is watching.
	FlattenOnStale bool
}

// DefaultLimits are deliberately small. Every one of them can be raised by a
// flag; none of them defaults to unlimited.
func DefaultLimits() Limits {
	return Limits{
		MaxDailyLossJPY:    1000,
		MaxNotionalJPY:     3000,
		MaxOpenNotionalJPY: 3000,
		MaxTradesPerDay:    200, // ~1,440 JPY of taker fees at 3,000 a trade
		MaxOrdersPerMinute: 10,
		StaleAfter:         15 * time.Second,
		FlattenOnStale:     true,
	}
}

// State is the live accounting the limits are checked against.
type State struct {
	Day            string // UTC date the counters belong to
	RealisedJPY    float64
	Trades         int
	OrdersInWindow []time.Time

	// Tripped records the first limit that fired, so the reason survives even
	// after the condition that caused it has passed.
	Tripped   string
	TrippedAt time.Time
}

// Guard applies the limits. It owns no clock: every method takes the time, so
// a test can run a whole trading day in a microsecond.
type Guard struct {
	lim   Limits
	state State
}

func New(lim Limits) *Guard { return &Guard{lim: lim} }

func (g *Guard) Limits() Limits { return g.lim }
func (g *Guard) State() State   { return g.state }

// rollDay resets the counters when the UTC date changes. The daily loss cap is
// a daily cap; a bot left running for a week must not still be held down by
// Monday's losses.
func (g *Guard) rollDay(now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if g.state.Day == day {
		return
	}
	g.state = State{Day: day}
}

// Fact is what the caller knows at this instant.
type Fact struct {
	Now time.Time

	// FeedAge is how long since any market data arrived.
	FeedAge time.Duration

	// Reconciled is false until the position has been confirmed against the
	// exchange. Trading before that risks doubling a position the bot forgot
	// it had — CLAUDE.md rule 7.
	Reconciled bool

	// OpenNotionalJPY is the exposure already carried.
	OpenNotionalJPY float64

	// PairTradable is the exchange's own answer: a suspended pair or a
	// circuit break means no order will be accepted anyway.
	PairTradable bool
}

// Allow returns what may happen now, and why.
//
// Order matters and mirrors decide.Compose: the facts that mean "we cannot see
// what we are doing" come before the limits that mean "we have done enough".
func (g *Guard) Allow(f Fact) (Verdict, string) {
	g.rollDay(f.Now)

	// Not yet reconciled: the bot does not know what it holds. It must not
	// place anything, in either direction — an exit sized from a guess is its
	// own accident.
	if !f.Reconciled {
		return Halt, "position not yet reconciled against the exchange"
	}

	// The feed is gone. Either flatten or stand still, but never open.
	if g.lim.StaleAfter > 0 && f.FeedAge > g.lim.StaleAfter {
		if g.lim.FlattenOnStale {
			return ExitOnly, fmt.Sprintf("no market data for %s; flattening", f.FeedAge.Round(time.Second))
		}
		return Halt, fmt.Sprintf("no market data for %s", f.FeedAge.Round(time.Second))
	}

	if !f.PairTradable {
		return Halt, "the exchange is not accepting orders on this pair"
	}

	// A previously tripped limit stays tripped for the day. Letting it clear
	// as soon as the condition passes would let a flapping value trade through
	// the cap a few JPY at a time.
	if g.state.Tripped != "" {
		return ExitOnly, g.state.Tripped
	}

	if g.lim.MaxDailyLossJPY > 0 && -g.state.RealisedJPY >= g.lim.MaxDailyLossJPY {
		return g.trip(f.Now, fmt.Sprintf("daily loss cap reached: %.2f of %.2f JPY",
			-g.state.RealisedJPY, g.lim.MaxDailyLossJPY))
	}
	if g.lim.MaxTradesPerDay > 0 && g.state.Trades >= g.lim.MaxTradesPerDay {
		return g.trip(f.Now, fmt.Sprintf("daily trade cap reached: %d", g.state.Trades))
	}
	if n := g.ordersInLastMinute(f.Now); g.lim.MaxOrdersPerMinute > 0 && n >= g.lim.MaxOrdersPerMinute {
		// Not sticky: a burst is a burst, and the next minute is allowed to be
		// normal. It still blocks entries while it lasts.
		return ExitOnly, fmt.Sprintf("order rate limit: %d in the last minute", n)
	}
	if g.lim.MaxOpenNotionalJPY > 0 && f.OpenNotionalJPY >= g.lim.MaxOpenNotionalJPY {
		return ExitOnly, fmt.Sprintf("exposure cap reached: %.0f of %.0f JPY",
			f.OpenNotionalJPY, g.lim.MaxOpenNotionalJPY)
	}
	return Trade, ""
}

func (g *Guard) trip(now time.Time, reason string) (Verdict, string) {
	g.state.Tripped, g.state.TrippedAt = reason, now
	return ExitOnly, reason
}

// EntryNotional is the size this entry may be, capped by both the per-order
// limit and the room left under the exposure cap.
func (g *Guard) EntryNotional(openJPY float64) float64 {
	want := g.lim.MaxNotionalJPY
	if g.lim.MaxOpenNotionalJPY > 0 {
		if room := g.lim.MaxOpenNotionalJPY - openJPY; room < want {
			want = room
		}
	}
	if want < 0 {
		return 0
	}
	return want
}

// RecordOrder notes that an order was submitted, for the rate limit.
func (g *Guard) RecordOrder(now time.Time) {
	g.rollDay(now)
	g.state.OrdersInWindow = append(g.state.OrdersInWindow, now)
	g.pruneOrders(now)
}

// RecordRoundTrip books a closed position's realised P&L against the day.
func (g *Guard) RecordRoundTrip(now time.Time, netJPY float64) {
	g.rollDay(now)
	g.state.RealisedJPY += netJPY
	g.state.Trades++
}

func (g *Guard) ordersInLastMinute(now time.Time) int {
	g.pruneOrders(now)
	return len(g.state.OrdersInWindow)
}

func (g *Guard) pruneOrders(now time.Time) {
	cutoff := now.Add(-time.Minute)
	kept := g.state.OrdersInWindow[:0]
	for _, t := range g.state.OrdersInWindow {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.state.OrdersInWindow = kept
}
