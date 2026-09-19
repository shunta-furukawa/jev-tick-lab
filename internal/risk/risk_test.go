package risk

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 19, 6, 0, 0, 0, time.UTC)

func ok() Fact {
	return Fact{Now: t0, FeedAge: time.Second, Reconciled: true, PairTradable: true}
}

func TestNothingIsPlacedBeforeThePositionIsReconciled(t *testing.T) {
	t.Parallel()
	// CLAUDE.md rule 7. A crash-restart that trusts local state will double a
	// position, and an exit sized from a guess is its own accident — so this
	// halts in both directions, not just entries.
	g := New(DefaultLimits())
	f := ok()
	f.Reconciled = false
	if v, why := g.Allow(f); v != Halt {
		t.Errorf("verdict = %q (%s), want halt", v, why)
	}
}

func TestLosingTheFeedFlattensRatherThanHoldingBlind(t *testing.T) {
	t.Parallel()
	// The laptop case: a closed lid is an open position nobody is watching.
	g := New(DefaultLimits())
	f := ok()
	f.FeedAge = time.Minute

	v, why := g.Allow(f)
	if v != ExitOnly {
		t.Fatalf("verdict = %q, want exit_only", v)
	}
	if !strings.Contains(why, "flatten") {
		t.Errorf("reason does not say it is flattening: %s", why)
	}

	// With flattening off it stands still instead, but it still never opens.
	lim := DefaultLimits()
	lim.FlattenOnStale = false
	if v, _ := New(lim).Allow(f); v != Halt {
		t.Errorf("verdict = %q, want halt when not flattening", v)
	}
}

func TestTheDailyLossCapStopsNewEntriesButNeverTrapsAPosition(t *testing.T) {
	t.Parallel()
	// A brake that cannot let you out is not a brake.
	g := New(DefaultLimits())
	g.RecordRoundTrip(t0, -600)
	if v, _ := g.Allow(ok()); v != Trade {
		t.Fatalf("600 of a 1000 cap already stopped trading")
	}

	g.RecordRoundTrip(t0, -420) // now 1020, past the cap
	v, why := g.Allow(ok())
	if v != ExitOnly {
		t.Fatalf("verdict = %q, want exit_only at the cap", v)
	}
	if !strings.Contains(why, "daily loss cap") {
		t.Errorf("reason = %q", why)
	}
}

func TestATrippedCapStaysTrippedForTheDay(t *testing.T) {
	t.Parallel()
	// Otherwise a winning trade lifts the brake and the bot trades back
	// through the cap a few JPY at a time.
	g := New(DefaultLimits())
	g.RecordRoundTrip(t0, -1200)
	if v, _ := g.Allow(ok()); v != ExitOnly {
		t.Fatal("cap did not trip")
	}

	g.RecordRoundTrip(t0.Add(time.Minute), +900) // back under the cap on paper
	f := ok()
	f.Now = t0.Add(2 * time.Minute)
	if v, why := g.Allow(f); v != ExitOnly {
		t.Errorf("verdict = %q (%s) — a winner un-tripped the daily cap", v, why)
	}
}

func TestTheCapResetsOnTheNextUTCDay(t *testing.T) {
	t.Parallel()
	g := New(DefaultLimits())
	g.RecordRoundTrip(t0, -5000)
	if v, _ := g.Allow(ok()); v != ExitOnly {
		t.Fatal("cap did not trip")
	}

	next := ok()
	next.Now = t0.Add(24 * time.Hour)
	if v, why := g.Allow(next); v != Trade {
		t.Errorf("verdict = %q (%s) — a daily cap must be daily", v, why)
	}
	if g.State().RealisedJPY != 0 || g.State().Trades != 0 {
		t.Errorf("counters survived the day roll: %+v", g.State())
	}
}

func TestTheTradeCountCapsTheFeeBleedThePerOrderSizeDoesNot(t *testing.T) {
	t.Parallel()
	// 3,000 JPY recycled 500 times pays 500 round trips of fees. The per-order
	// size bounds none of that; this is the limit that does.
	lim := DefaultLimits()
	lim.MaxTradesPerDay = 3
	g := New(lim)

	for i := 0; i < 3; i++ {
		g.RecordRoundTrip(t0, 0) // break even: no loss, but fees were paid
	}
	v, why := g.Allow(ok())
	if v != ExitOnly {
		t.Fatalf("verdict = %q, want exit_only at the trade cap", v)
	}
	if !strings.Contains(why, "trade cap") {
		t.Errorf("reason = %q", why)
	}
}

func TestARunawayLoopIsStoppedEvenWhenItMovesNoPosition(t *testing.T) {
	t.Parallel()
	// Submit-and-cancel in a loop trips none of the position or P&L limits and
	// still pays for every crossing.
	lim := DefaultLimits()
	lim.MaxOrdersPerMinute = 5
	g := New(lim)

	for i := 0; i < 5; i++ {
		g.RecordOrder(t0.Add(time.Duration(i) * time.Second))
	}
	f := ok()
	f.Now = t0.Add(6 * time.Second)
	if v, why := g.Allow(f); v != ExitOnly {
		t.Fatalf("verdict = %q (%s), want the rate limit to bite", v, why)
	}

	// And it clears once the burst ages out: a burst is a burst.
	f.Now = t0.Add(2 * time.Minute)
	if v, _ := g.Allow(f); v != Trade {
		t.Error("the rate limit did not clear after the window passed")
	}
}

func TestExposureIsCappedSeparatelyFromTheOrderSize(t *testing.T) {
	t.Parallel()
	// With no pyramiding these are equal, but enforcing only the order size
	// means a pyramiding bug becomes an unbounded position.
	lim := DefaultLimits()
	lim.MaxNotionalJPY, lim.MaxOpenNotionalJPY = 3000, 3000
	g := New(lim)

	f := ok()
	f.OpenNotionalJPY = 3000
	if v, why := g.Allow(f); v != ExitOnly {
		t.Errorf("verdict = %q (%s), want exit_only when fully exposed", v, why)
	}

	// And the next entry is sized by the room left, not by the flat maximum.
	if got := g.EntryNotional(2000); got != 1000 {
		t.Errorf("EntryNotional(2000) = %v, want 1000", got)
	}
	if got := g.EntryNotional(5000); got != 0 {
		t.Errorf("EntryNotional over the cap = %v, want 0", got)
	}
}

func TestASuspendedPairHalts(t *testing.T) {
	t.Parallel()
	g := New(DefaultLimits())
	f := ok()
	f.PairTradable = false
	if v, _ := g.Allow(f); v != Halt {
		t.Errorf("verdict = %q, want halt", v)
	}
}

func TestBlindnessIsCheckedBeforeExhaustion(t *testing.T) {
	t.Parallel()
	// Ordering mirrors decide.Compose: "we cannot see what we are doing" must
	// win over "we have done enough", or a tripped cap would mask a dead feed.
	g := New(DefaultLimits())
	g.RecordRoundTrip(t0, -9999)

	f := ok()
	f.Reconciled = false
	f.FeedAge = time.Hour
	v, why := g.Allow(f)
	if v != Halt || !strings.Contains(why, "reconcile") {
		t.Errorf("verdict = %q (%s), want the reconciliation halt to win", v, why)
	}
}

func TestDefaultsAreSmallAndNoneIsUnlimited(t *testing.T) {
	t.Parallel()
	// Every one of these can be raised by a flag. None of them may default to
	// "no limit", because the default is what runs when someone is in a hurry.
	l := DefaultLimits()
	for name, v := range map[string]float64{
		"MaxDailyLossJPY":    l.MaxDailyLossJPY,
		"MaxNotionalJPY":     l.MaxNotionalJPY,
		"MaxOpenNotionalJPY": l.MaxOpenNotionalJPY,
		"MaxTradesPerDay":    float64(l.MaxTradesPerDay),
		"MaxOrdersPerMinute": float64(l.MaxOrdersPerMinute),
	} {
		if v <= 0 {
			t.Errorf("%s defaults to %v, which is unlimited", name, v)
		}
	}
	if l.StaleAfter <= 0 || !l.FlattenOnStale {
		t.Error("the dead-man switch must be on by default")
	}
}
