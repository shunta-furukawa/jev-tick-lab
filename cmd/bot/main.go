// Command bot runs the Jev day-trading reproduction experiment.
//
// Phases (see CLAUDE.md). Controlled by -mode:
//
//	observe  stream + state only, no model calls        (phase 1)
//	shadow   stream + Jev + full logging, NO trading    (phase 2, the default)
//	paper    shadow + simulated fills                   (phase 4, not implemented)
//	live     real orders                                (phase 5, not implemented)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/obs"
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
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log, *pair, *mode, *model, *logDir, *tick, *timeout, *minHist, *printState); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, pair, mode, model, logDir string, tick, timeout, minHist time.Duration, printState bool) error {
	switch mode {
	case "observe", "shadow":
	case "paper":
		return fmt.Errorf("paper mode is not implemented; see CLAUDE.md phase 4")
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
		}); err != nil {
			return fmt.Errorf("write run header: %w", err)
		}
	}

	client := jev.New(apiKey, model)

	// Position is flat here only because shadow mode never trades. In live mode
	// this MUST be reconciled from the exchange before the first tick.
	var position marketstate.Position

	// inFlight guarantees at most one outstanding evaluation. If a call is slow,
	// the tick is skipped rather than queued — a queued answer describes a market
	// that no longer exists.
	var inFlight atomic.Bool
	var skipped, evaluated atomic.Int64

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	log.Info("started", "run_id", runID, "pair", pair, "mode", mode, "model", model, "tick", tick.String())

	// Warmup transitions are logged once each, not every second: a feed that
	// never syncs must be visible, and one that flaps must not drown the log.
	var ready, warnedNotReady bool

	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down", "run_id", runID, "evaluated", evaluated.Load(), "skipped", skipped.Load())
			// In live mode, flatten here before returning.
			return nil

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

			go func(now time.Time, snap marketstate.Snapshot, pos marketstate.Position) {
				defer inFlight.Store(false)
				evaluate(ctx, log, client, logger, questions, thresholds, now, snap, pos, evalOpts{
					runID:   runID,
					model:   model,
					mode:    mode,
					timeout: timeout,
				})
				evaluated.Add(1)
			}(now.UTC(), snap, position)
		}
	}
}

type evalOpts struct {
	runID   string
	model   string
	mode    string
	timeout time.Duration
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
	rec.Signal = decide.Compose(now, time.Now().UTC(), snap, resp.Answers, pos, thresholds)

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

	// Phase 4 hands rec.Signal to the paper executor here. Nothing consumes it
	// in shadow mode by design: phase 2 answers the interesting question
	// without any execution code at all.
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
