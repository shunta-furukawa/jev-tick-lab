package exec

import (
	"testing"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
)

// answers that clear every gate and call for a long entry.
func buyAnswers() map[string]jev.Answer {
	return map[string]jev.Answer{
		jev.QAnomaly:    {Type: "noul", Noul: 0.05},
		jev.QFakeout:    {Type: "noul", Noul: 0.05},
		jev.QHoldRisk:   {Type: "score", Score: 0.5},
		jev.QEntryScore: {Type: "score", Score: 3.9},
		jev.QAction: {Type: "choice", Choice: jev.ActionBuy, Confidence: 0.9,
			Probabilities: map[string]float64{jev.ActionBuy: 0.9}},
	}
}

func traderCfg() Config {
	c := DefaultConfig("xrp_jpy")
	c.Latency = 0
	c.NotionalJPY = 2075 // ~10 units at 207.5
	return c
}

func TestBothPathsActOnOneAnswerSet(t *testing.T) {
	t.Parallel()
	tr := NewTrader(traderCfg(), decide.DefaultThresholds())

	sig, pos, problems := tr.Decide(now0, now0, snap(), buyAnswers())
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if sig.Intent != decide.IntentOpenLong {
		t.Fatalf("intent = %q, gate = %q", sig.Intent, sig.Gate)
	}
	if !pos.IsFlat() {
		t.Error("the position reported is the one the signal was composed against, which was flat")
	}

	st := tr.State(207.51)
	if len(st) != 2 || st[0].Style != "taker" || st[1].Style != "maker" {
		t.Fatalf("state = %+v, want taker then maker in a stable order", st)
	}
	for _, s := range st {
		if s.Working != 1 {
			t.Errorf("%s has %d working orders, want 1", s.Style, s.Working)
		}
	}
}

func TestTheTakerFillsAndTheMakerWaits(t *testing.T) {
	t.Parallel()
	// The divergence that makes running both worth the code: crossing is
	// certain, resting is not.
	b := marketstate.NewBook("xrp_jpy")
	if err := b.ApplyDepthWhole(wholeJSON()); err != nil {
		t.Fatal(err)
	}

	tr := NewTrader(traderCfg(), decide.DefaultThresholds())
	if _, _, p := tr.Decide(now0, now0, snap(), buyAnswers()); len(p) != 0 {
		t.Fatalf("problems: %v", p)
	}

	execs, _ := tr.Pump(b, now0.Add(time.Second))
	if len(execs) != 1 {
		t.Fatalf("executions = %d, want just the taker", len(execs))
	}
	if execs[0].Fill.Style != Taker {
		t.Errorf("the path that filled was %q", execs[0].Fill.Style)
	}

	st := tr.State(207.51)
	if st[0].Position.IsFlat() {
		t.Error("the taker path should be long")
	}
	if !st[1].Position.IsFlat() {
		t.Error("the maker path should still be waiting in the queue")
	}
	// And the taker has already paid for the privilege.
	if st[0].FeesJPY <= 0 {
		t.Errorf("taker fees = %.4f, want a charge", st[0].FeesJPY)
	}
}

func TestAPrintIsNeverCountedTwiceAcrossPumps(t *testing.T) {
	t.Parallel()
	// The cursor is the only thing standing between this simulator and
	// double-filling every maker order it has.
	b := marketstate.NewBook("xrp_jpy")
	if err := b.ApplyDepthWhole(wholeJSON()); err != nil {
		t.Fatal(err)
	}
	cfg := traderCfg()
	cfg.NotionalJPY = 207.5 * 5 // 5 units, well inside one print
	tr := NewTrader(cfg, decide.DefaultThresholds())
	tr.Decide(now0, now0, snap(), buyAnswers())

	// Clear the whole queue at the bid and then some, in one print.
	if err := b.ApplyTransactions(txJSON(1, now0.Add(time.Second), "sell", 207.50, 1000)); err != nil {
		t.Fatal(err)
	}

	first, _ := tr.Pump(b, now0.Add(2*time.Second))
	makers := 0
	for _, e := range first {
		if e.Fill.Style == Maker {
			makers++
		}
	}
	if makers != 1 {
		t.Fatalf("maker fills on the first pump = %d, want 1", makers)
	}

	second, _ := tr.Pump(b, now0.Add(3*time.Second))
	for _, e := range second {
		if e.Fill.Style == Maker {
			t.Fatal("the same print filled the maker order again on the next pump")
		}
	}
}

func TestAnUnsyncedBookFillsNothing(t *testing.T) {
	t.Parallel()
	// Same rule decide.Compose applies: a book no depth_whole has seeded is
	// not a market, and a fill taken against it is invented.
	b := marketstate.NewBook("xrp_jpy")
	tr := NewTrader(traderCfg(), decide.DefaultThresholds())
	tr.Decide(now0, now0, snap(), buyAnswers())

	execs, cancels := tr.Pump(b, now0.Add(time.Minute))
	if len(execs) != 0 || len(cancels) != 0 {
		t.Fatalf("filled against an unseeded book: %v %v", execs, cancels)
	}
}

func TestShutdownCancelsOrdersButNeverClosesPositions(t *testing.T) {
	t.Parallel()
	// A run that liquidates on Ctrl-C reports a P&L that depended on when you
	// pressed it.
	b := marketstate.NewBook("xrp_jpy")
	if err := b.ApplyDepthWhole(wholeJSON()); err != nil {
		t.Fatal(err)
	}
	tr := NewTrader(traderCfg(), decide.DefaultThresholds())
	tr.Decide(now0, now0, snap(), buyAnswers())
	tr.Pump(b, now0.Add(time.Second)) // taker fills, maker still resting

	cancels := tr.Flatten(now0.Add(2*time.Second), "shutdown")
	if len(cancels) != 1 || cancels[0].Style != Maker {
		t.Fatalf("cancels = %+v, want the resting maker order", cancels)
	}
	st := tr.State(207.51)
	if st[0].Position.IsFlat() {
		t.Error("shutdown closed the taker position; it should still be open and marked")
	}
	if _, total := tr.paths[Taker].ledger.Wins(); total != 0 {
		t.Error("shutdown booked a round trip that never happened")
	}
}
