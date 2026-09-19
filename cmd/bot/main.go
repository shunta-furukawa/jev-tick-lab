// Command bot runs the Jev day-trading reproduction experiment.
//
// Phases (see CLAUDE.md). Controlled by -mode:
//
//	observe  stream + state only, no model calls        (phase 1)
//	shadow   stream + Jev + full logging, NO trading    (phase 2, the default)
//	paper    shadow + simulated fills, no real orders   (phase 4)
//	live     REAL ORDERS, real money                    (phase 5)
//
// Live mode needs -i-understand-this-spends-real-money. There is no default
// that trades: arming it is a thing you type, once, deliberately.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/bitbank"
	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/exec"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
	"github.com/shunta-furukawa/jev-tick-lab/internal/report"
	"github.com/shunta-furukawa/jev-tick-lab/internal/risk"
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

		// Live mode. The brakes are flags so they can be tightened without a
		// rebuild; none of them defaults to unlimited, because the default is
		// what runs when someone is in a hurry.
		armed        = flag.Bool("i-understand-this-spends-real-money", false, "required for -mode live")
		liveStyle    = flag.String("live-style", "taker", "live: taker crosses the spread and fills; maker rests at the touch and often does not")
		maxDailyLoss = flag.Float64("max-daily-loss-jpy", 1000, "live: stop opening once the day's realised loss reaches this")
		maxTrades    = flag.Int("max-trades-per-day", 200, "live: caps the fee bleed, which the order size does not")
		maxOrdersMin = flag.Int("max-orders-per-minute", 10, "live: the runaway brake")
		staleAfter   = flag.Duration("flatten-after", 15*time.Second, "live: flatten when the feed has been silent this long")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	paperCfg := exec.DefaultConfig(*pair)
	paperCfg.NotionalJPY = *notional
	paperCfg.Latency = *orderLatency
	paperCfg.MakerTimeout = *makerTimeout
	paperCfg.CrossAfterTimeout = *crossAfter

	limits := risk.DefaultLimits()
	limits.MaxDailyLossJPY = *maxDailyLoss
	limits.MaxNotionalJPY = *notional
	limits.MaxOpenNotionalJPY = *notional
	limits.MaxTradesPerDay = *maxTrades
	limits.MaxOrdersPerMinute = *maxOrdersMin
	limits.StaleAfter = *staleAfter

	if err := run(log, *pair, *mode, *model, *logDir, *tick, *timeout, *minHist, *printState, *serve,
		paperCfg, limits, exec.Style(*liveStyle), *armed); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, pair, mode, model, logDir string, tick, timeout, minHist time.Duration,
	printState bool, serveAddr string, paperCfg exec.Config, limits risk.Limits, liveStyle exec.Style, armed bool) error {
	switch mode {
	case "observe", "shadow", "paper":
	case "live":
		if !armed {
			return fmt.Errorf("live mode places real orders with real money. " +
				"Re-run with -i-understand-this-spends-real-money if that is what you want")
		}
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

	// Live mode: establish the truth before anything else, and refuse to start
	// if it cannot be established. CLAUDE.md rule 7.
	var live *exec.Live
	var guard *risk.Guard
	if mode == "live" {
		var err error
		live, guard, err = setUpLive(ctx, log, logger, pair, runID, liveStyle, limits)
		if err != nil {
			return err
		}
		defer func() {
			// Withdraw anything resting. Open positions are deliberately left
			// open: liquidating on shutdown reports a P&L that depended on
			// when the process was killed.
			shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := live.CancelWorking(shutCtx); err != nil {
				log.Error("could not cancel the working order on shutdown — CHECK THE EXCHANGE", "err", err)
			}
			if pos := live.Position(); !pos.IsFlat() {
				log.Warn("exiting with an open position, which is left as it is",
					"side", pos.Side, "size", pos.Size, "entry", pos.EntryPrice)
			}
		}()
	}

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
	if trader != nil || live != nil {
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
			now := at.UTC()
			if trader != nil {
				execs, cancels := trader.Pump(book, now)
				for _, e := range execs {
					recordFill(log, logger, runID, pair, mode, e)
				}
				for _, c := range cancels {
					recordCancel(log, logger, runID, pair, mode, c)
				}
			}
			if live != nil {
				execs, err := live.Poll(ctx, now)
				if err != nil {
					log.Error("poll the working order", "err", err)
				}
				for _, e := range execs {
					recordFill(log, logger, runID, pair, mode, e)
					if e.Closed != nil {
						guard.RecordRoundTrip(now, e.Closed.NetJPY)
					}
				}
				// The dead-man switch. A feed that has gone quiet means the
				// bot is holding something it cannot see — on a laptop, that
				// is usually a closed lid.
				snap := book.Snapshot(now)
				if v, why := guard.Allow(liveFact(now, snap, live, limits)); v == risk.ExitOnly &&
					!live.Position().IsFlat() && !live.Working() && snap.Stale {
					log.Warn("flattening", "reason", why)
					if _, err := live.Submit(ctx, now, decide.Signal{At: now, Intent: decide.IntentClose},
						snap, v, limits.MaxNotionalJPY); err != nil {
						log.Error("could not flatten", "err", err)
					} else {
						guard.RecordOrder(now)
					}
				}
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
					runID:    runID,
					model:    model,
					mode:     mode,
					timeout:  timeout,
					trader:   trader,
					live:     live,
					guard:    guard,
					notional: limits.MaxNotionalJPY,
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

	// live and guard are nil outside live mode. guard decides whether a
	// signal is allowed to become an order at all.
	live     *exec.Live
	guard    *risk.Guard
	notional float64
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
	switch {
	case opts.live != nil:
		rec.Signal = decide.Compose(now, decidedAt, snap, resp.Answers, pos, thresholds)
		placeLive(ctx, log, opts, rec.Signal, snap, decidedAt)
		rec.Paper = []exec.StyleState{liveState(opts, snap, decidedAt)}

	case opts.trader == nil:
		rec.Signal = decide.Compose(now, decidedAt, snap, resp.Answers, pos, thresholds)

	default:
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

// setUpLive prepares real trading, or refuses to.
//
// Everything here is a precondition, not a nicety. The order is deliberate:
// read the venue's own rules, then find out what is actually held, and only
// then allow a decision to become an order.
func setUpLive(ctx context.Context, log *slog.Logger, logger *obs.Logger,
	pair, runID string, style exec.Style, limits risk.Limits) (*exec.Live, *risk.Guard, error) {

	key, secret := os.Getenv("BITBANK_API_KEY"), os.Getenv("BITBANK_API_SECRET")
	if key == "" || secret == "" {
		return nil, nil, fmt.Errorf("live mode needs BITBANK_API_KEY and BITBANK_API_SECRET in the environment " +
			"(never on the command line, never in a file this repo can see)")
	}

	// The venue's own rules: fees, the minimum size, the rounding, and whether
	// it is accepting orders at all. Fetched rather than configured, so the
	// numbers cannot be stale.
	rules, err := bitbank.FetchPairRules(ctx, bitbank.DefaultEndpoint, pair)
	if err != nil {
		return nil, nil, fmt.Errorf("read the pair rules: %w", err)
	}
	if !rules.TradingAllowed() {
		return nil, nil, fmt.Errorf("bitbank is not accepting orders on %s right now", pair)
	}
	log.Info("venue rules",
		"pair", rules.Name, "maker_bps", rules.MakerFeeBps, "taker_bps", rules.TakerFeeBps,
		"min_size", rules.UnitAmount, "price_digits", rules.PriceDigits, "amount_digits", rules.AmountDigits)

	client := bitbank.New(key, secret)

	// Rule 7. Never trust local state for what is held.
	rec, err := risk.Reconcile(ctx, client, pair, rules.UnitAmount)
	if err != nil {
		return nil, nil, fmt.Errorf("reconcile against the exchange: %w", err)
	}
	for _, note := range rec.Notes {
		log.Warn("reconciliation", "note", note)
	}
	log.Info("reconciled",
		"position_size", rec.Position.Size, "base_free", rec.BaseFree,
		"quote_free", rec.QuoteFree, "cancelled_orders", len(rec.Cancelled))

	if rec.QuoteFree < limits.MaxNotionalJPY && rec.Position.IsFlat() {
		log.Warn("the account holds less than one order's worth of JPY; entries will be rejected",
			"jpy_free", rec.QuoteFree, "order_size", limits.MaxNotionalJPY)
	}

	cfg := exec.DefaultLiveConfig(pair, rules)
	cfg.Style = style
	l := exec.NewLive(client, cfg)
	l.Adopt(rec.Position)

	log.Warn("LIVE MODE ARMED — this places real orders with real money",
		"pair", pair, "style", string(style),
		"order_size_jpy", limits.MaxNotionalJPY,
		"max_daily_loss_jpy", limits.MaxDailyLossJPY,
		"max_trades_per_day", limits.MaxTradesPerDay,
		"flatten_after", limits.StaleAfter.String())

	return l, risk.New(limits), nil
}

// liveFact assembles what the risk layer needs to know right now.
func liveFact(now time.Time, snap marketstate.Snapshot, live *exec.Live, limits risk.Limits) risk.Fact {
	pos := live.Position()
	open := pos.Size * snap.Last

	age := time.Duration(0)
	if snap.Stale {
		// Snapshot reports staleness as a boolean rather than an age, so
		// report just past the threshold: enough to trip the brake, not enough
		// to claim a precision the snapshot does not have.
		age = limits.StaleAfter + time.Second
	}
	return risk.Fact{
		Now:     now,
		FeedAge: age,
		// The position came from the exchange at startup and has been tracked
		// by the executor since. Rule 7 is satisfied by setUpLive refusing to
		// start otherwise.
		Reconciled:      true,
		OpenNotionalJPY: open,
		PairTradable:    !snap.Halted() && snap.BookSynced,
	}
}

// placeLive turns a signal into a real order, if the brakes allow it.
func placeLive(ctx context.Context, log *slog.Logger, opts evalOpts,
	sig decide.Signal, snap marketstate.Snapshot, now time.Time) {

	if sig.Intent == decide.IntentNone {
		return
	}
	verdict, why := opts.guard.Allow(liveFact(now, snap, opts.live, opts.guard.Limits()))
	if verdict != risk.Trade && sig.Intent != decide.IntentClose {
		log.Info("entry withheld", "verdict", string(verdict), "reason", why, "intent", string(sig.Intent))
		return
	}

	size := opts.guard.EntryNotional(opts.live.Position().Size * snap.Last)
	if sig.Intent != decide.IntentClose && size <= 0 {
		log.Info("entry withheld", "reason", "no room under the exposure cap")
		return
	}

	order, err := opts.live.Submit(ctx, now, sig, snap, verdict, size)
	switch {
	case errors.Is(err, exec.ErrBusy):
		// Skip, never queue: a second order on a stale decision is the
		// accident this whole design is arranged to avoid.
		log.Info("skipped", "reason", "an order is already working")
	case errors.Is(err, exec.ErrShortOnSpot):
		log.Info("skipped", "reason", "the model called a short and this is spot")
	case err != nil:
		log.Error("ORDER REJECTED", "err", err, "intent", string(sig.Intent))
	case order != nil:
		opts.guard.RecordOrder(now)
		log.Warn("ORDER PLACED", "order_id", order.OrderID, "side", order.Side,
			"type", order.Type, "amount", order.StartAmount, "price", order.Price,
			"intent", string(sig.Intent), "reason", sig.Reason)
	}
}

// liveState reports the live path in the same shape the paper paths use, so
// the record schema and the dashboard do not need a second variant.
func liveState(opts evalOpts, snap marketstate.Snapshot, now time.Time) exec.StyleState {
	l := opts.live
	pos := l.Position()
	led := l.Ledger()
	wins, trips := led.Wins()

	working := 0
	if l.Working() {
		working = 1
	}
	// The verdict and the reason a brake is on are the two things worth
	// seeing on a page about real money, so they ride in the same fields the
	// paper paths use for intent and gate.
	verdict, why := opts.guard.Allow(liveFact(now, snap, l, opts.guard.Limits()))
	if why == "" {
		why = "no brake on"
	}
	return exec.StyleState{
		Style:     "live",
		Intent:    string(verdict),
		Gate:      why,
		Position:  pos,
		Working:   working,
		RoundTrip: trips,
		Wins:      wins,
		NetJPY:    led.NetJPY(),
		FeesJPY:   led.FeesJPY(),
		UnrealJPY: led.MarkToMarket(pos.EntryPrice),
	}
}
