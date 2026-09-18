// Command preflight makes exactly one real call to TypeSafe and checks what
// comes back.
//
//	export TYPESAFE_API_KEY=...
//	go run ./cmd/preflight -pair xrp_jpy -model jev-1.13.0
//
// Run this before committing to a multi-day collection. Nothing else in this
// repository has ever talked to the real API: internal/jev is tested against a
// local fake, and jev.Validate only checks the question set's shape without
// sending it. So the first real answer to "does the API accept this?" would
// otherwise arrive on the first tick of the run itself, and a 422 there means
// days of records containing nothing but an error string.
//
// It costs one call — about $0.00006 — and reports the measured token count and
// latency, which turns the cost estimate and the call timeout from assumptions
// into numbers.
//
// Exit 0 means the contract holds. Exit 1 means it does not.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/shunta-furukawa/jev-tick-lab/internal/decide"
	"github.com/shunta-furukawa/jev-tick-lab/internal/jev"
	"github.com/shunta-furukawa/jev-tick-lab/internal/marketstate"
	"github.com/shunta-furukawa/jev-tick-lab/internal/stream"
)

func main() {
	var (
		pair        = flag.String("pair", "xrp_jpy", "bitbank pair to build the state from")
		model       = flag.String("model", "jev-1.13.0", "the versioned model id to pin")
		tick        = flag.Duration("tick", 3*time.Second, "the cadence the run will use, so the latency verdict is about your deployment")
		minHist     = flag.Duration("min-history", time.Minute, "wait for this much bar history before sending; a cold state is smaller, so measuring tokens against one undercounts")
		warmup      = flag.Duration("warmup", 2*time.Minute, "how long to wait for a usable market snapshot")
		callTimeout = flag.Duration("call-timeout", 10*time.Second, "deadline for the one call; deliberately looser than the bot's 3s")
		stateFile   = flag.String("state", "", "send this file as the state instead of connecting to bitbank")
		showState   = flag.Bool("show-state", false, "print the state text that was sent")
		pricePerM   = flag.Float64("price-per-mtok", 0.042, "USD per million input tokens, for the cost projection")

		// So that the half of this that needs no credentials can be exercised
		// without spending a call, or handing a key to anything.
		dryRun = flag.Bool("dry-run", false, "build and print the state, then stop without calling the API")
	)
	flag.Parse()

	if err := run(*pair, *model, *tick, *minHist, *warmup, *callTimeout, *stateFile, *showState, *pricePerM, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "\npreflight FAILED:", err)
		os.Exit(1)
	}
}

func run(pair, model string, tick, minHist, warmup, callTimeout time.Duration, stateFile string, showState bool, pricePerM float64, dryRun bool) error {
	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" && !dryRun {
		return fmt.Errorf("TYPESAFE_API_KEY is not set; this command exists to make a real call. Use -dry-run to check everything up to the call")
	}

	questions := jev.QuestionSet()
	if err := jev.Validate(questions); err != nil {
		return fmt.Errorf("the question set is malformed before it even left: %w", err)
	}
	fmt.Printf("question set  %d questions, hash-stable, locally valid\n", len(questions))

	ctx, cancel := context.WithTimeout(context.Background(), warmup+callTimeout+10*time.Second)
	defer cancel()

	state, err := buildState(ctx, pair, minHist, warmup, stateFile)
	if err != nil {
		return err
	}
	fmt.Printf("state         %d bytes\n", len(state))
	if showState || dryRun {
		fmt.Printf("\n%s\n", state)
	}

	if dryRun {
		fmt.Printf("dry run       stopping here; this is exactly what would be sent as `state`\n")
		return nil
	}

	client := jev.New(apiKey, model)
	callCtx, cancelCall := context.WithTimeout(ctx, callTimeout)
	defer cancelCall()

	fmt.Printf("\ncalling %s as %s ...\n", jev.DefaultEndpoint, model)
	started := time.Now()
	resp, err := client.Ask(callCtx, state, questions)
	latency := time.Since(started)

	if err != nil {
		// The error body is the useful part: a 422 names the offending field.
		return fmt.Errorf("after %s: %w", latency.Round(time.Millisecond), err)
	}

	problems, notes := jev.Verify(questions, model, resp)
	report(resp, questions, latency, tick, pricePerM, notes)

	if len(problems) > 0 {
		fmt.Println("\nproblems:")
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
		return fmt.Errorf("%d problem(s) with the response", len(problems))
	}

	fmt.Println("\nOK — the API accepts this question set and answered all of it.")
	return nil
}

// buildState renders a real market state, because sending something synthetic
// would leave the one thing preflight is for — does the real payload work —
// untested.
func buildState(ctx context.Context, pair string, minHist, warmup time.Duration, stateFile string) (string, error) {
	if stateFile != "" {
		b, err := os.ReadFile(stateFile)
		return string(b), err
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	book := marketstate.NewBook(pair)
	sc := stream.New(log,
		stream.TickerRoom(pair),
		stream.DepthWholeRoom(pair),
		stream.DepthDiffRoom(pair),
		stream.TransactionsRoom(pair),
		stream.CircuitBreakInfoRoom(pair),
	)
	go sc.Run(ctx)

	whole := stream.DepthWholeRoom(pair)
	diff := stream.DepthDiffRoom(pair)
	tx := stream.TransactionsRoom(pair)
	ticker := stream.TickerRoom(pair)
	breaker := stream.CircuitBreakInfoRoom(pair)

	deadline := time.After(warmup)
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()

	fmt.Printf("stream        connecting to %s, waiting up to %s for a seeded book and %s of history\n", pair, warmup, minHist)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()

		case <-deadline:
			return "", fmt.Errorf("no usable snapshot after %s (need %s of history); run `go run ./cmd/dump` to see what the stream is doing", warmup, minHist)

		case ev := <-sc.Events:
			switch ev.Room {
			case ticker:
				_ = book.ApplyTicker(ev.Data)
			case whole:
				_ = book.ApplyDepthWhole(ev.Data)
			case diff:
				_ = book.ApplyDepthDiff(ev.Data)
			case tx:
				_ = book.ApplyTransactions(ev.Data)
			case breaker:
				_ = book.ApplyCircuitBreak(ev.Data)
			}

		case <-poll.C:
			snap := book.Snapshot(time.Now().UTC())
			// The same warmup condition the bot uses: a price, a book that a
			// depth_whole has seeded, and enough history that the state text is
			// not mostly "still building".
			if snap.Last == 0 || !snap.BookSynced || snap.HistorySeconds() < int(minHist.Seconds()) {
				continue
			}
			fmt.Printf("stream        warmed up: last %.4f, spread %.3f bps, %ds of history\n",
				snap.Last, snap.SpreadBps, snap.HistorySeconds())
			return marketstate.Render(snap, marketstate.Position{}), nil
		}
	}
}

func report(resp *jev.Response, questions map[string]jev.Question, latency, tick time.Duration, pricePerM float64, notes []string) {
	fmt.Printf("\nmodel         %s answered\n", resp.Model)
	fmt.Printf("latency       %s\n", latency.Round(time.Millisecond))
	fmt.Printf("tokens        %d in, %d out\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)

	if resp.Usage.InputTokens > 0 {
		perCall := float64(resp.Usage.InputTokens) * pricePerM / 1e6
		callsPerDay := 86400 / tick.Seconds()
		fmt.Printf("cost          $%.6f per call -> $%.2f/day at %s, $%.2f for a 5-day run\n",
			perCall, perCall*callsPerDay, tick, perCall*callsPerDay*5)
	}

	// The measured latency has to clear three separate bars, and only one of
	// them is obvious.
	maxAge := decide.DefaultThresholds().MaxDecisionAge
	switch {
	case latency > 3*time.Second:
		fmt.Printf("              WARNING: slower than the bot's 3s call timeout — every tick would fail\n")
	case latency > maxAge:
		// The quiet one. decide gates any answer older than MaxDecisionAge, so
		// a run like this collects answers and produces no signals at all —
		// every record shows gate=decision_age.
		fmt.Printf("              WARNING: slower than Thresholds.MaxDecisionAge (%s), so decide would gate\n"+
			"                       EVERY answer as stale. Raise MaxDecisionAge, or accept a\n"+
			"                       dataset in which no signal ever passes gate 0.\n", maxAge)
	case latency > tick:
		fmt.Printf("              NOTE: slower than the %s tick, so ticks will be skipped while a call is in flight\n", tick)
	}

	fmt.Println("\nanswers:")
	for _, id := range jev.IDs(questions) {
		a := resp.Answers[id]
		switch questions[id].Type {
		case "noul":
			fmt.Printf("  %-14s noul  %.3f\n", id, a.Noul)
		case "choice":
			fmt.Printf("  %-14s %-12s p=%.3f conf=%.3f\n", id, a.Choice, a.Probabilities[a.Choice], a.Confidence)
		case "score":
			fmt.Printf("  %-14s score %.2f  conf=%.3f\n", id, a.Score, a.Confidence)
		}
	}

	if len(notes) > 0 {
		fmt.Println("\nnotes (not failures, but worth a look):")
		for _, n := range notes {
			fmt.Printf("  - %s\n", n)
		}
	}
}
