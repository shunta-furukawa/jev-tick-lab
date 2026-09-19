package exec

import (
	"sync"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// Trader runs the same decisions down two execution paths at once.
//
// Maker and taker are not variants of a strategy, they are different
// strategies: one pays 24bps a round trip and always gets filled, the other is
// paid 4bps and often does not get filled at all. Running one and quoting the
// other's fees is how a paper result becomes meaningless. Running both against
// the identical answer set makes the difference the only variable.
//
// decide.Compose is pure, so it is evaluated once per path against that path's
// own position. The maker book and the taker book genuinely diverge — a maker
// entry that never filled leaves that path flat while the taker path is long —
// and the gates that read the position (pyramid, flat) must see the truth for
// the path they are gating.
//
// Everything here is mutex-guarded: the tick loop pumps the book from the main
// goroutine while evaluations land from their own.
type Trader struct {
	mu sync.Mutex

	cfg    Config
	thr    decide.Thresholds
	paths  map[Style]*path
	cursor int64 // print cursor, so no print is counted twice or missed

	// The last ladder seen by Pump, so a maker order submitted between pumps
	// can still be told what is resting ahead of it.
	bids, asks []marketstate.Level
	synced     bool
}

type path struct {
	sim    *Simulator
	ledger *Ledger
	last   decide.Signal
}

func NewTrader(cfg Config, thr decide.Thresholds) *Trader {
	t := &Trader{cfg: cfg, thr: thr, paths: map[Style]*path{}}
	for _, st := range []Style{Taker, Maker} {
		t.paths[st] = &path{sim: New(cfg), ledger: NewLedger(st)}
	}
	return t
}

// Styles is the order the paths are reported in, so a table never reorders.
func Styles() []Style { return []Style{Taker, Maker} }

// SeedCursor starts the print cursor at the current end of the buffer, so the
// first tick does not replay whatever was already there.
func (t *Trader) SeedCursor(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cursor = id
}

// Decide composes a signal per path and submits any resulting order.
//
// It returns the taker path's signal, which is what the tick record carries:
// the taker path always fills, so its position is the one a reader can
// reconstruct the gates from without also knowing what a passive order did.
// The maker path's signal and position are reported by State.
func (t *Trader) Decide(now, decidedAt time.Time, snap marketstate.Snapshot, ans map[string]jev.Answer) (decide.Signal, marketstate.Position, []error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var problems []error
	for _, st := range Styles() {
		p := t.paths[st]
		pos := p.ledger.Position()
		sig := decide.Compose(now, decidedAt, snap, ans, pos, t.thr)
		p.last = sig

		if sig.Intent == decide.IntentNone {
			continue
		}
		o, err := p.sim.Submit(sig, st, snap, pos, decidedAt)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if o != nil && st == Maker {
			if bids, asks, synced := t.book(); synced {
				p.sim.SeedQueue(o, bids, asks)
			}
		}
	}

	taker := t.paths[Taker]
	return taker.last, taker.ledger.Position(), problems
}

// book is set by Pump so SeedQueue can see the ladder without the trader
// holding a reference to the Book itself.
func (t *Trader) book() (bids, asks []marketstate.Level, synced bool) {
	return t.bids, t.asks, t.synced
}

// Execution is one fill, plus the round trip it closed if it closed one.
// They travel together because the P&L of a closing fill is meaningless
// without the entry it is being measured against.
type Execution struct {
	Fill   Fill
	Closed *RoundTrip
}

// Pump advances both paths with the current book and everything that has
// printed since the last call. It returns what filled and what gave up.
func (t *Trader) Pump(b *marketstate.Book, now time.Time) ([]Execution, []Cancel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// The Book has its own lock and never calls back into the Trader, so
	// reading it from under this one cannot cycle.
	bids, asks, synced := b.Ladder(0)
	prints, next := b.TradesAfter(t.cursor)
	t.bids, t.asks, t.synced = bids, asks, synced
	t.cursor = next

	if !synced {
		// The book is not trustworthy, so neither is any fill taken against
		// it. Orders keep waiting; they do not fill on a guess.
		return nil, nil
	}

	var execs []Execution
	var cancels []Cancel
	for _, st := range Styles() {
		p := t.paths[st]
		if len(prints) > 0 {
			for _, f := range p.sim.OnPrints(prints, now) {
				execs = append(execs, Execution{Fill: f, Closed: p.ledger.Apply(f)})
			}
		}
		fs, cs := p.sim.OnBook(bids, asks, now)
		for _, f := range fs {
			execs = append(execs, Execution{Fill: f, Closed: p.ledger.Apply(f)})
		}
		cancels = append(cancels, cs...)
	}
	return execs, cancels
}

// Flatten gives up every working order. It does NOT close open positions: a
// paper run that liquidates at shutdown reports a P&L that depended on when
// you pressed Ctrl-C.
func (t *Trader) Flatten(now time.Time, reason string) []Cancel {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Cancel
	for _, st := range Styles() {
		out = append(out, t.paths[st].sim.CancelAll(now, reason)...)
	}
	return out
}

// StyleState is one path's standing, for the record and the dashboard.
type StyleState struct {
	Style     string               `json:"style"`
	Intent    string               `json:"intent"`
	Gate      string               `json:"gate"`
	Position  marketstate.Position `json:"position"`
	Working   int                  `json:"working"`
	RoundTrip int                  `json:"round_trips"`
	Wins      int                  `json:"wins"`
	NetJPY    float64              `json:"net_jpy"`
	FeesJPY   float64              `json:"fees_jpy"`
	UnrealJPY float64              `json:"unrealized_jpy"`
}

// State reports both paths, in a stable order.
func (t *Trader) State(last float64) []StyleState {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]StyleState, 0, len(t.paths))
	for _, st := range Styles() {
		p := t.paths[st]
		wins, trips := p.ledger.Wins()
		out = append(out, StyleState{
			Style:     string(st),
			Intent:    string(p.last.Intent),
			Gate:      string(p.last.Gate),
			Position:  p.ledger.Position(),
			Working:   p.sim.Working(),
			RoundTrip: trips,
			Wins:      wins,
			NetJPY:    p.ledger.NetJPY(),
			FeesJPY:   p.ledger.FeesJPY(),
			UnrealJPY: p.ledger.MarkToMarket(last),
		})
	}
	return out
}

// Position is the taker path's position. It is what the state text describes
// and what the logged Signal is composed against.
//
// The maker path can hold something different — an entry that never filled
// leaves it flat while the taker path is long — and this is the one place the
// two-path design is lossy: both paths read answers produced from a state text
// describing the taker position. It matters only for the questions that read a
// position at all (hold_risk, and take/stop in trader_action); the market
// reads are unaffected. The alternative is two model calls per tick, which
// doubles the bill to remove a caveat rather than a result.
func (t *Trader) Position() marketstate.Position {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paths[Taker].ledger.Position()
}

// RoundTrips is every closed position on a path, for the report.
func (t *Trader) RoundTrips(st Style) []RoundTrip {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paths[st].ledger.RoundTrips()
}
