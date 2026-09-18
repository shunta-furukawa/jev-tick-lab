// Command bot runs the Jev day-trading reproduction experiment.
//
// Phases (see CLAUDE.md). Controlled by -mode:
//
//	observe  stream + state only, no model calls        (phase 1)
//	shadow   stream + Jev + full logging, NO trading    (phase 2, the default)
//	paper    shadow + simulated fills                   (phase 4)
//	live     real orders                                (phase 5, not implemented)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
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
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" && *mode != "observe" {
		log.Error("TYPESAFE_API_KEY is not set")
		os.Exit(1)
	}
	if *mode == "live" {
		log.Error("live mode is not implemented; see CLAUDE.md phase 5")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	book := marketstate.NewBook(*pair)
	sc := stream.New(log,
		stream.DepthWholeRoom(*pair),
		stream.DepthDiffRoom(*pair),
		stream.TransactionsRoom(*pair),
	)
	go sc.Run(ctx)
	go ingest(ctx, log, sc, book, *pair)

	logger, err := obs.NewLogger(*logDir)
	if err != nil {
		log.Error("open log", "err", err)
		os.Exit(1)
	}
	defer logger.Close()

	client := jev.New(apiKey, *model)
	questions := jev.QuestionSet()
	thresholds := decide.DefaultThresholds()

	// Position is flat here only because paper mode starts flat. In live mode
	// this MUST be reconciled from the exchange before the first tick.
	var position marketstate.Position

	// inFlight guarantees at most one outstanding evaluation. If a call is slow,
	// the tick is skipped rather than queued — a queued answer describes a market
	// that no longer exists.
	var inFlight atomic.Bool

	ticker := time.NewTicker(*tick)
	defer ticker.Stop()

	log.Info("started", "pair", *pair, "mode", *mode, "model", *model, "tick", tick.String())

	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			// In live mode, flatten here before returning.
			return

		case now := <-ticker.C:
			snap := book.Snapshot(now)
			if snap.Last == 0 {
				continue // not warmed up yet
			}
			if *mode == "observe" {
				log.Info("snapshot",
					"last", snap.Last, "spread_bps", snap.SpreadBps,
					"ret60s", snap.Ret60s, "imbalance", snap.DepthImbalance)
				continue
			}
			if !inFlight.CompareAndSwap(false, true) {
				log.Warn("evaluation still in flight, skipping tick")
				continue
			}

			go func(now time.Time, snap marketstate.Snapshot, pos marketstate.Position) {
				defer inFlight.Store(false)
				evaluate(ctx, log, client, logger, questions, thresholds, now, snap, pos, *model, *mode)
			}(now, snap, position)
		}
	}
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
	modelRequested string,
	mode string,
) {
	state := marketstate.Render(snap, pos)
	sum := sha256.Sum256([]byte(state))

	rec := obs.Record{
		TickID:         now.UTC().Format(time.RFC3339Nano),
		At:             now,
		Pair:           snap.Pair,
		ModelRequested: modelRequested,
		Snapshot:       snap,
		StateText:      state,
		StateHash:      hex.EncodeToString(sum[:]),
	}

	// Hard deadline: an answer that arrives after the tick window is useless.
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	started := time.Now()
	resp, err := client.Ask(callCtx, state, questions)
	rec.LatencyMs = float64(time.Since(started).Microseconds()) / 1000

	if err != nil {
		rec.Error = err.Error()
		_ = logger.Write(rec)
		log.Warn("jev call failed", "err", err, "latency_ms", rec.LatencyMs)
		return
	}

	rec.ModelVersion = resp.Model
	rec.Answers = resp.Answers
	rec.InputTokens = resp.Usage.InputTokens
	rec.OutputTokens = resp.Usage.OutputTokens
	rec.Signal = decide.Compose(now, resp.Answers, pos, thresholds)

	if err := logger.Write(rec); err != nil {
		log.Error("log write", "err", err)
	}

	log.Info("evaluated",
		"intent", rec.Signal.Intent,
		"action", rec.Signal.ActionChoice,
		"prob", rec.Signal.ActionProb,
		"conf", rec.Signal.ActionConf,
		"anomaly", rec.Signal.AnomalyNoul,
		"reason", rec.Signal.Reason,
		"latency_ms", rec.LatencyMs,
		"model", resp.Model,
	)

	if mode == "paper" {
		// TODO(phase 4): hand rec.Signal to the paper executor.
		_ = rec.Signal
	}
}

func ingest(ctx context.Context, log *slog.Logger, sc *stream.Client, book *marketstate.Book, pair string) {
	whole := stream.DepthWholeRoom(pair)
	diff := stream.DepthDiffRoom(pair)
	tx := stream.TransactionsRoom(pair)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sc.Events:
			var err error
			switch ev.Room {
			case whole:
				err = book.ApplyDepthWhole(ev.Data)
			case diff:
				err = book.ApplyDepthDiff(ev.Data)
			case tx:
				err = book.ApplyTransactions(ev.Data)
			}
			if err != nil {
				log.Warn("apply event", "room", ev.Room, "err", err)
			}
		}
	}
}
