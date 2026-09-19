// Command bot runs the Jev day-trading reproduction experiment.
//
// Phases (see CLAUDE.md). Controlled by -mode:
//
//	observe  stream + state only, no model calls        (phase 1)
//	shadow   stream + Jev + full logging, NO trading    (phase 2, the default)
//	paper    shadow + simulated fills, no real orders   (phase 4)
//	live     real orders                                (phase 5, not implemented)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/exec"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
	"github.com/shunta-furukawa/jev-tick-lab/internal/report"
	"github.com/shunta-furukawa/jev-tick-lab/internal/stream"
)

func main() {
	var (
		pair    = flag.String("pair", "xrp_jpy", "bitbank trading pair")
		mode    = flag.String("mode", "shadow", "observe | shadow | paper | live")
		model   = flag.String("model", "jev-1.13.0", "TypeSafe model id — pin a version, never use an alias in a recorded run")
		logDir  = flag.String("log-dir", "./data", "directory for JSONL tick logs")
		tick    = flag.Duration("tick", time.Second, "evaluation cadence")
		minHist = flag.Duration("min-history", time.Minute, "do not evaluate until the bar series is at least this long")
		timeout = flag.Duration("call-timeout", 3*time.Second, "hard deadline for one Jev evaluation")
		// Phase 1's exit criterion is "the state text renders correctly against
		// live data", which needs a way to actually look at it.
		printState = flag.Bool("print-state", false, "in observe mode, print the rendered state text each tick")

		// Watching a collection through three CLI tools is a chore, and a chore
		// you will not do is a collection you are not really watching.
		serve = flag.String("serve", "", "also serve the live dashboard here, e.g. 127.0.0.1:8080")

		// Paper mode only. Sizes and fees are configuration because both move:
		// the bitbank maker rebate is a campaign, and a simulator with either
		// welded in keeps reporting a strategy that stopped existing.
		notional     = flag.Float64("notional-jpy", 10000, "paper: size of one entry, in yen")
		orderLatency = flag.Duration("order-latency", 150*time.Millisecond, "paper: round trip to bitbank; an order does not exist until it arrives")
		makerTimeout = flag.Duration("maker-timeout", 30*time.Second, "paper: cancel a resting order that has not filled")
		crossAfter   = flag.Bool("cross-after-timeout", false, "paper: take the spread when a maker entry times out")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	paperCfg := exec.DefaultConfig(*pair)
	paperCfg.NotionalJPY = *notional
	paperCfg.Latency = *orderLatency
	paperCfg.MakerTimeout = *makerTimeout
	paperCfg.CrossAfterTimeout = *crossAfter

	if err := run(log, *pair, *mode, *model, *logDir, *tick, *timeout, *minHist, *printState, *serve, paperCfg); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, pair, mode, model, logDir string, tick, timeout, minHist time.Duration, printState bool, serveAddr string, paperCfg exec.Config) error {
	switch mode {
	case "observe", "shadow", "paper":
	case "live":
		return fmt.Errorf("live mode is not implemented; see CLAUDE.md phase 5")
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}

	questions := jev.QuestionSet()
	if err := jev.Validate(questions); err != nil {
		return err
	}

	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" && mode != "observe" {
		return fmt.Errorf("TYPESAFE_API_KEY is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	book := marketstate.NewBook(pair)
	sc := stream.New(log,
		stream.TickerRoom(pair),
		stream.DepthWholeRoom(pair),
		stream.DepthDiffRoom(pair),
		stream.TransactionsRoom(pair),
		stream.CircuitBreakInfoRoom(pair),
	)
	go sc.Run(ctx)
	go ingest(ctx, log, sc, book, pair)

	started := time.Now().UTC()
	runID := obs.NewRunID(started)
	thresholds := decide.DefaultThresholds()

	// Observe mode writes no records, so it does not open a log at all — an
	// empty ticks file for a run that never evaluated anything is a trap for
	// the analysis pass.
	var logger *obs.Logger
	if mode != "observe" {
		var err error
		if logger, err = obs.NewLogger(logDir); err != nil {
			return fmt.Errorf("open log: %w", err)
		}
		defer logger.Close()

		// The run header is what makes a logged tick interpretable months
		// later: which thresholds, which question set, which model was asked,
		// and which build produced the state text they were asked about.
		revision, buildTime, modified := obs.BuildInfo()
		log.Info("build", "revision", revision, "modified", modified)

		var paper *exec.Config
		if mode == "paper" {
			paper = &paperCfg
		}
		if err := logger.WriteRun(obs.Run{
			RunID:          runID,
			StartedAt:      started,
			Pair:           pair,
			Mode:           mode,
			ModelRequested: model,
			TickInterval:   tick.String(),
			BuildRevision:  revision,
			BuildTime:      buildTime,
			BuildModified:  modified,
			Thresholds:     thresholds,
			QuestionIDs:    jev.IDs(questions),
			QuestionsHash:  obs.HashQuestions(questions),
			Questions:      questions,
			Paper:          paper,
		}); err != nil {
			return fmt.Errorf("write run header: %w", err)
		}
	}

	// The dashboard reads the same files the logger writes, so it never touches
	// the tick loop. An HTTP handler that panics is recovered by net/http and
	// cannot take the collector down with it.
	if serveAddr != "" {
		if mode == "observe" {
			log.Warn("observe mode writes no records, so the dashboard will stay empty", "addr", serveAddr)
		}
		srv := &http.Server{
			Addr: serveAddr,
			Handler: report.Handler(logDir, report.Options{
				TickInterval: tick,
				HorizonSec:   report.DefaultOptions().HorizonSec,
				BandBps:      report.DefaultOptions().BandBps,
				Buckets:      report.DefaultOptions().Buckets,
				PricePerMTok: report.DefaultOptions().PricePerMTok,
			}),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("dashboard", "url", "http://"+serveAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("dashboard stopped", "err", err)
			}
		}()
		defer srv.Close()
	}

	client := jev.New(apiKey, model)

	// Position is flat here only because shadow mode never trades. In live mode
	// this MUST be reconciled from the exchange before the first tick — see
	// rule 7 in CLAUDE.md. Paper mode owns its position inside the trader,
	// which starts flat because nothing it simulates is real.
	var position marketstate.Position

	var trader *exec.Trader
	if mode == "paper" {
		trader = exec.NewTrader(paperCfg, thresholds)
		trader.SeedCursor(book.LatestTradeID())
		log.Info("paper mode",
			"notional_jpy", paperCfg.NotionalJPY,
			"maker_bps", paperCfg.Fees.MakerBps, "taker_bps", paperCfg.Fees.TakerBps,
			"order_latency", paperCfg.Latency.String(),
			"maker_timeout", paperCfg.MakerTimeout.String(),
			"allow_short", paperCfg.AllowShort)
	}

	// inFlight guarantees at most one outstanding evaluation. If a call is slow,
	// the tick is skipped rather than queued — a queued answer describes a market
	// that no longer exists.
	var inFlight atomic.Bool
	var skipped, evaluated atomic.Int64

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	// Fills are pumped far more often than decisions are taken. An order
	// submitted at t fills against the book as it actually moved, not against
	// whatever it happened to be at the next evaluation — at a 3s cadence that
	// difference is most of the fill.
	var pumpC <-chan time.Time
	if trader != nil {
		pump := time.NewTicker(100 * time.Millisecond)
		defer pump.Stop()
		pumpC = pump.C
	}
	// A nil channel blocks forever in a select, so outside paper mode the arm
	// is simply never taken — rather than firing against a nil trader, which
	// would panic in the one loop CLAUDE.md says must never panic.

	log.Info("started", "run_id", runID, "pair", pair, "mode", mode, "model", model, "tick", tick.String())

	// Warmup transitions are logged once each, not every second: a feed that
	// never syncs must be visible, and one that flaps must not drown the log.
	var ready, warnedNotReady bool

	for {
		select {
		case <-ctx.Done():
			if trader != nil {
				// Cancel working orders, but never close open positions: a run
				// that liquidates on Ctrl-C reports a P&L that depended on when
				// you pressed it.
				for _, c := range trader.Flatten(time.Now().UTC(), "shutdown") {
					recordCancel(log, logger, runID, pair, mode, c)
				}
				for _, st := range trader.State(0) {
					log.Info("paper result", "style", st.Style,
						"round_trips", st.RoundTrip, "wins", st.Wins,
						"net_jpy", st.NetJPY, "fees_jpy", st.FeesJPY,
						"open", !st.Position.IsFlat())
				}
			}
			log.Info("shutting down", "run_id", runID, "evaluated", evaluated.Load(), "skipped", skipped.Load())
			// In live mode, flatten here before returning.
			return nil

		case at := <-pumpC:
			execs, cancels := trader.Pump(book, at.UTC())
			for _, e := range execs {
				recordFill(log, logger, runID, pair, mode, e)
			}
			for _, c := range cancels {
				recordCancel(log, logger, runID, pair, mode, c)
			}

		case now := <-ticker.C:
			snap := book.Snapshot(now.UTC())

			// A tick without a price, or without a book that a depth_whole has
			// seeded, has nothing to judge.
			//
			// Nor does one without history. The series starts empty, so for the
			// first minute most of the state text is "n/a, still building" —
			// honest, but not worth paying a model to read. Waiting also keeps
			// the dataset free of records that describe a market nobody could
			// have formed a view on.
			history := snap.HistorySeconds()
			if snap.Last == 0 || !snap.BookSynced || history < int(minHist.Seconds()) {
				if ready || !warnedNotReady {
					log.Warn("not ready",
						"last", snap.Last, "book_synced", snap.BookSynced,
						"history_s", history, "need_history_s", int(minHist.Seconds()),
						"stale", snap.Stale)
					warnedNotReady = true
				}
				ready = false
				continue
			}
			if !ready {
				log.Info("warmed up", "last", snap.Last, "spread_bps", snap.SpreadBps, "history_s", history)
				ready, warnedNotReady = true, false
			}

			if mode == "observe" {
				if printState {
					fmt.Print("\n" + marketstate.Render(snap, position) + "\n")
				}
				log.Info("snapshot",
					"last", snap.Last, "spread_bps", snap.SpreadBps,
					"ret60s", snap.Ret60s, "imbalance", snap.DepthImbalance,
					"trades_30s", snap.Trades30s, "circuit_break", snap.CircuitBreak)
				continue
			}
			if !inFlight.CompareAndSwap(false, true) {
				skipped.Add(1)
				log.Warn("evaluation still in flight, skipping tick")
				continue
			}

			// The state text has to describe the position the model is being
			// asked about. In paper mode that is the trader's, not the flat
			// placeholder shadow mode uses.
			pos := position
			if trader != nil {
				pos = trader.Position()
			}

			go func(now time.Time, snap marketstate.Snapshot, pos marketstate.Position) {
				defer inFlight.Store(false)
				evaluate(ctx, log, client, logger, questions, thresholds, now, snap, pos, evalOpts{
					runID:   runID,
					model:   model,
					mode:    mode,
					timeout: timeout,
					trader:  trader,
				})
				evaluated.Add(1)
			}(now.UTC(), snap, pos)
		}
	}
}

type evalOpts struct {
	runID   string
	model   string
	mode    string
	timeout time.Duration

	// trader is nil outside paper mode. When it is set it owns the position
	// and the gating, because the gates that read a position must see the
	// path's real one.
	trader *exec.Trader
}

func evaluate(
	ctx context.Context,
	log *slog.Logger,
	client *jev.Client,
	logger *obs.Logger,
	questions map[string]jev.Question,
	thresholds decide.Thresholds,
	now time.Time,
	snap marketstate.Snapshot,
	pos marketstate.Position,
	opts evalOpts,
) {
	state := marketstate.Render(snap, pos)
	sum := sha256.Sum256([]byte(state))

	rec := obs.Record{
		TickID:         now.UTC().Format(time.RFC3339Nano),
		RunID:          opts.runID,
		At:             now,
		Pair:           snap.Pair,
		Mode:           opts.mode,
		ModelRequested: opts.model,
		Snapshot:       snap,
		Position:       pos,
		StateText:      state,
		StateHash:      hex.EncodeToString(sum[:]),
	}

	// Hard deadline: an answer that arrives after the tick window is useless.
	callCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	started := time.Now()
	resp, err := client.Ask(callCtx, state, questions)
	rec.LatencyMs = float64(time.Since(started).Microseconds()) / 1000

	if err != nil {
		// A failed call is still a record. Gaps in the log are themselves data,
		// but only if the gap is labelled.
		rec.Error = err.Error()
		if writeErr := logger.Write(rec); writeErr != nil {
			log.Error("log write", "err", writeErr)
		}
		log.Warn("jev call failed", "err", err, "latency_ms", rec.LatencyMs)
		return
	}

	rec.ModelVersion = resp.Model
	rec.Answers = resp.Answers
	rec.InputTokens = resp.Usage.InputTokens
	rec.OutputTokens = resp.Usage.OutputTokens

	decidedAt := time.Now().UTC()
	if opts.trader == nil {
		rec.Signal = decide.Compose(now, decidedAt, snap, resp.Answers, pos, thresholds)
	} else {
		// The trader composes once per execution path, because the maker and
		// taker books diverge and the position gates must see the truth for
		// the path they gate. The record carries the taker path's signal and
		// the position it was composed against, so it still re-scores exactly.
		var problems []error
		rec.Signal, rec.Position, problems = opts.trader.Decide(now, decidedAt, snap, resp.Answers)
		rec.Paper = opts.trader.State(snap.Last)
		for _, p := range problems {
			// A short on spot is the expected one: the model is asked a
			// symmetric question and half the market is not tradeable here.
			log.Warn("order not placed", "err", p, "intent", rec.Signal.Intent)
		}
	}

	if err := logger.Write(rec); err != nil {
		log.Error("log write", "err", err)
	}

	log.Info("evaluated",
		"intent", rec.Signal.Intent,
		"gate", rec.Signal.Gate,
		"action", rec.Signal.ActionChoice,
		"prob", rec.Signal.ActionProb,
		"conf", rec.Signal.ActionConf,
		"anomaly", rec.Signal.AnomalyNoul,
		"reason", rec.Signal.Reason,
		"latency_ms", rec.LatencyMs,
		"model", resp.Model,
	)

	// Fills are not applied here. The trader is pumped from the tick loop at
	// 100ms, because an order submitted now fills against the book as it
	// actually moves — not against whatever it happens to be at the next
	// evaluation, which at a 3s cadence would be most of the fill.
}

func ingest(ctx context.Context, log *slog.Logger, sc *stream.Client, book *marketstate.Book, pair string) {
	var (
		ticker  = stream.TickerRoom(pair)
		whole   = stream.DepthWholeRoom(pair)
		diff    = stream.DepthDiffRoom(pair)
		tx      = stream.TransactionsRoom(pair)
		breaker = stream.CircuitBreakInfoRoom(pair)
	)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sc.Events:
			var err error
			switch ev.Room {
			case ticker:
				err = book.ApplyTicker(ev.Data)
			case whole:
				err = book.ApplyDepthWhole(ev.Data)
			case diff:
				err = book.ApplyDepthDiff(ev.Data)
			case tx:
				err = book.ApplyTransactions(ev.Data)
			case breaker:
				err = book.ApplyCircuitBreak(ev.Data)
			}
			if err != nil {
				log.Warn("apply event", "room", ev.Room, "err", err)
			}
		}
	}
}

// recordFill writes one simulated execution to the fills log and says so in
// the journal. A fill that only exists in memory is not a result.
func recordFill(log *slog.Logger, logger *obs.Logger, runID, pair, mode string, e exec.Execution) {
	f := e.Fill
	rec := obs.FillRecord{
		RunID: runID, At: f.At, Pair: pair, Mode: mode,
		Style: string(f.Style), Side: f.Side, Intent: string(f.Intent), OrderID: f.OrderID,
		Price: f.Price, Size: f.Size, Notional: f.Notional,
		FeeJPY: f.FeeJPY, SlipBps: f.SlipBps, WaitedMs: f.WaitedMs,
	}
	if e.Closed != nil {
		rec.NetJPY, rec.NetBps, rec.HeldSec = e.Closed.NetJPY, e.Closed.NetBps, e.Closed.HeldSec
	}
	if err := logger.WriteFill(rec); err != nil {
		log.Error("fill log write", "err", err)
	}

	args := []any{
		"style", rec.Style, "side", rec.Side, "intent", rec.Intent,
		"price", rec.Price, "size", rec.Size, "fee_jpy", rec.FeeJPY,
	}
	if f.Style == exec.Maker {
		args = append(args, "waited_ms", rec.WaitedMs)
	} else {
		args = append(args, "slip_bps", rec.SlipBps)
	}
	if e.Closed != nil {
		args = append(args, "net_jpy", rec.NetJPY, "net_bps", rec.NetBps, "held_s", rec.HeldSec)
	}
	log.Info("fill", args...)
}

// recordCancel writes an order that never filled. These are the whole point of
// simulating a maker path: the trades a passive strategy simply does not get
// are invisible everywhere else.
func recordCancel(log *slog.Logger, logger *obs.Logger, runID, pair, mode string, c exec.Cancel) {
	rec := obs.FillRecord{
		RunID: runID, At: c.At, Pair: pair, Mode: mode,
		Style: string(c.Style), OrderID: c.OrderID,
		Cancelled: true, Unfilled: c.Unfilled, Reason: c.Reason,
	}
	if err := logger.WriteFill(rec); err != nil {
		log.Error("fill log write", "err", err)
	}
	log.Info("cancelled", "style", rec.Style, "unfilled", rec.Unfilled, "reason", rec.Reason)
}
