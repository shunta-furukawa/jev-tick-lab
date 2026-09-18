# CLAUDE.md

Context for agents working in this repository. Read this fully before writing code.

---

## What this project is

An experiment: **can TypeSafe's Jev model reproduce the snap judgements of a
short-term day trader, evaluated once per second, and is its confidence actually
calibrated on a task it was never trained for?**

The trading P&L is not the deliverable. The deliverable is a dataset and an
analysis. A run that loses money but produces a clean calibration curve is a
success. A run that makes money with no logged confidence data is a failure.

Target market: bitbank (Japanese exchange), spot, JPY pairs.
Language: Go. Deployment: single VM in `asia-northeast1`.

---

## Non-negotiable rules

These exist because violating them silently invalidates the whole experiment.
Do not relax them without the owner explicitly saying so in the current session.

### 1. Never build a backtest against historical price data

Jev's training data plausibly includes the actual price history of these
markets. Asking it "what happens next" about 2024 or 2025 data leaks the answer.
Any backtest result would be fiction, and publishing it would be worse than
publishing nothing.

**Forward-only.** Run it live, log everything, analyse afterwards. If asked for
a backtest, push back and explain this rather than building one.

### 2. Never ask the model something code can compute exactly

Moving averages, spreads, depth ratios, breakout levels, returns, volatility —
all computed in `internal/marketstate` and embedded in the state text as
numbers. Jev is asked only for judgement.

If you find yourself adding a question like "what is the 20-period average" or
"has price crossed the high", stop: that belongs in `marketstate`.

### 3. Every Choice question needs an escape option

Closed output sets only produce reliability when the set actually covers the
input space. Without an `unclear` / `wait` / `none` option, the model is forced
to pick the least-wrong label for states that fit nothing.

### 4. Noul answers have no `confidence` field

Only Choice and Score carry `confidence`. Code that assumes `answer.confidence`
exists on every answer will silently read zero for binary questions. See
`internal/jev.Answer`.

### 5. Pin the model version in any recorded run

`jev-latest` resolves to whatever is current and **moves without notice**. A
moved model invalidates every tuned threshold. Use a versioned id
(`jev-1.13.0`) via `-model`, and always log `response.model`, which reports the
version that actually answered.

### 6. Question ids are a schema

The constants in `internal/jev/questions.go` are the column names of the
dataset. Renaming one breaks comparison against every record already logged.
Append new questions; do not rename or repurpose old ones. Adding questions is
nearly free — they are evaluated in parallel against the same state.

### 7. Position state is never trusted from local storage

On startup in any mode that can trade, reconcile against bitbank's actual
balance and open-order endpoints. A crash-restart that trusts a stale local
file will double up a position.

---

## Architecture

```
bitbank public WS ──▶ stream ──▶ marketstate (book, 1s bars, indicators)
  ticker                                │
  depth_whole / depth_diff              │  every 1s  Render() → state text
  transactions                          ▼
  circuit_break_info               jev.Ask()  (async, max 1 in flight)
                                        │
                                        ▼
                                 decide.Compose()  ← all weights live here
                                        │
                        ┌───────────────┴───────────────┐
                        ▼                               ▼
                 obs.Logger (JSONL)              executor (paper/live)
                        │
                        ▼
              cmd/fill ──▶ cmd/calib (phase 3)
```

### Package responsibilities

| Package | Owns | Never does |
|---|---|---|
| `internal/stream` | Socket.IO framing, reconnect, room subscription | Any interpretation of prices |
| `internal/marketstate` | Order book, 1s bars, all indicator maths, state rendering | Any network I/O |
| `internal/jev` | HTTP client, question definitions | Any decision logic |
| `internal/decide` | Thresholds, gating order, signal composition | Any I/O; must stay pure and testable |
| `internal/obs` | JSONL records, rotation | Any blocking work in the hot path |
| `internal/exec` | Fill simulation, order placement | Reading Jev answers directly — it consumes `decide.Signal` only |
| `internal/calib` | Reliability bins, Brier, ECE, outcome definitions | Any I/O; `cmd/calib` reads the files |
| `internal/health` | Whether a running collection is still producing usable data | Any I/O; `cmd/logcheck` reads the files and picks the exit code |

| Command | Does |
|---|---|
| `cmd/bot` | The tick loop. `-mode observe\|shadow` |
| `cmd/dump` | Prints raw stream frames. Does not import `internal/stream`, so it shows the wire rather than our reading of it |
| `cmd/fill` | Forward-fill: joins each logged tick to the price 10s/60s/300s later |
| `cmd/calib` | The calibration report: stated probability against realised frequency |
| `cmd/logcheck` | Hourly health verdict on the collection. Exit 0 healthy, 1 degraded, 2 could not tell |

### Key design decisions and why

**Jev calls are asynchronous with at most one in flight.** A 1s tick against a
~400ms model leaves headroom, but a slow call must skip the tick rather than
queue. A queued answer describes a market that no longer exists. See
`inFlight` in `cmd/bot/main.go`.

**Decision freshness is bounded.** `Thresholds.MaxDecisionAge` (2s). Anything
older is discarded rather than acted on.

**Brakes are evaluated before accelerators.** `decide.Compose` checks the
anomaly gate and the hold-risk gate before any entry logic can run. This
ordering is deliberate — Jev's role in this system is primarily a brake, because
its calibration on financial judgement is exactly what is unproven.

**Code-computed facts brake before model judgement does.** `decide.Compose`
checks, in this order: is the answer still fresh, can we see the market at all
(feed stale, or a book never seeded by a `depth_whole`), and does the exchange
itself say this market is halted. Only then does anything Jev said get a vote.
All three are facts off the wire, so asking about them would violate rule 2.

**The spread is a gate, not a question.** An all-taker round trip on a JPY alt
is 24bps. `Thresholds.MaxSpreadBps` blocks new entries when the book is wider
than crossing it can pay for. It deliberately does not block exits: a brake
that traps a position is not a brake.

**Every signal carries a `Gate`.** Free-text `Reason` is for a human reading the
log; `Gate` is a stable enum, so "how often did each brake fire" is a group-by
rather than a string match. Treat it like a question id: append, never rename.

**Everything is logged, including failures.** A failed call writes a record with
`error` set. Gaps in the log are themselves data. Each run also writes one row
to `runs-YYYY-MM-DD.jsonl` with its thresholds, question set and hash, so a tick
recorded months ago is still interpretable.

**Health is a property of the data, not of the process.** systemd restarts a
dead bot. It cannot see the failure that actually costs this experiment its
deliverable: a bot that is alive, writing a record every second, and writing
records nobody can analyse — evaluated against a book that never re-synced, or
against a model version that moved halfway through the run. `cmd/logcheck` runs
hourly and fails on that. See [docs/operations.md](docs/operations.md).

---

## The TypeSafe API

```
POST https://api.typesafe.ai/v1/systemone
Authorization: Bearer $TYPESAFE_API_KEY
Content-Type: application/json
```

Request:
```json
{
  "state": "...text or structured data...",
  "model": "jev-1.13.0",
  "questions": {
    "my_id": { "type": "noul", "instructions": "...", "criteria": {"true": "...", "false": "..."} }
  }
}
```

Question types and their `criteria` shapes:

| type | criteria | answer fields |
|---|---|---|
| `noul` | `{"true": "...", "false": "..."}` (optional) | `noul` (0–1) |
| `choice` | `{option: description or null}` (required) | `choice`, `probabilities`, `confidence` |
| `score` | ordered `["level0", "level1", ...]`, min 2 (required) | `score`, `legend`, `probabilities`, `confidence` |

Response:
```json
{
  "model": "jev-1.13.0",
  "answers": { "my_id": { "type": "noul", "noul": 0.92 } },
  "usage": { "input_tokens": 312, "output_tokens": 48 }
}
```

`score` is a probability-weighted value across the levels, so it lands between
integers. Read `probabilities` when the distribution shape matters, not just the
point value.

Errors: `401` bad key, `422` malformed request (body names the offending field),
`429` rate limit, `529` overloaded. Back off exponentially on 429/529; do not
retry 401/422.

Rate limits at time of writing: **1,200 req/min, 250,000 tokens/sec**.
A 1s cadence uses 60 rpm. TypeSafe warns these limits move without notice.

Docs: <https://docs.typesafe.ai> — the full page index is at
<https://docs.typesafe.ai/llms.txt>. Relevant reading: `/primitives`,
`/confidence`, `/patterns/confidence-routing`, `/patterns/composite-scoring`,
and `/model-jaggedness/jev-1.13` (the vendor's own list of known weak spots).

---

## bitbank specifics

**Public stream** is Socket.IO v4 over WebSocket at
`wss://stream.bitbank.cc/socket.io/?EIO=4&transport=websocket`.
`internal/stream` implements only the frames needed (`0`, `40`, `2`/`3`, `42`).
Rooms: `ticker_{pair}`, `transactions_{pair}`, `depth_whole_{pair}`,
`depth_diff_{pair}`.

✅ **Verified against a live connection on 2026-09-17.** Room names, handshake
and payload fields are as implemented; the captured frames and the list of what
the verification changed are in [docs/stream-verification.md](docs/stream-verification.md).
Re-check with `go run ./cmd/dump` after any exchange-side change.

**Depth handling**: `depth_whole` is a full snapshot, `depth_diff` is
incremental with amount `0` meaning delete. Both carry the same sequence id
(`sequenceId` on the whole, `s` on the diff), **as a JSON string**, despite the
docs table calling it a number.

Sequence ids rise monotonically but are explicitly *not* consecutive, so a gap
cannot be detected by arithmetic. bitbank's own algorithm, which
`marketstate.Book` implements, is:

1. Buffer diffs; do not serve a book that no `depth_whole` has seeded.
2. On a whole: replace the book, then replay only the buffered diffs whose `s`
   exceeds its `sequenceId`, in ascending order. Wholes arrive delayed relative
   to diffs, so without the replay the book loses that window.
3. Ignore any diff at or below the book's current sequence.

A whole is not optional housekeeping: diffs only cover ~200 levels from the
best bid and ask, so the periodic whole is the only thing that drops a level
that fell out of range.

**Trades are sparse.** Over a 30s sample, `btc_jpy` printed once and `xrp_jpy`
not at all. The 1s bar series is therefore continuous in wall-clock seconds
with carried-forward closes, not one bar per print — otherwise "the last 60
bars" spans minutes while the state text calls it 60 seconds. The `ticker` room
is what keeps the series alive on a quiet pair.

**Fees** (as of the 2026-02-02 schedule; a maker-rebate campaign, so re-check):

| | maker | taker |
|---|---|---|
| BTC/JPY | 0.00% | 0.10% |
| other JPY pairs | −0.02% | 0.12% |

Round trip all-taker on an alt is **24bps**. All-maker is **+4bps**. This gap
dominates any short-horizon strategy: a perfectly accurate predictor of a 20bps
move still loses money taking liquidity. Fee rates must be configuration, never
hardcoded constants, because the rebate campaign can end.

---

## Infrastructure

`terraform/` builds the machine and its surroundings; `deploy/deploy.sh` puts
code on it. They are separate so that a code deploy can never touch the bucket
holding collected data, and a VM rebuild never needs a code change.

The runbook is [docs/operations.md](docs/operations.md). Three things about it
are load-bearing:

- **The API key never touches the disk.** `deploy/run.sh` fetches it from Secret
  Manager into the process environment and `exec`s the bot. Not an
  `EnvironmentFile`, not the image, not Terraform state. An earlier version of
  the README claimed this while the unit read a plaintext file; the claim is now
  true.
- **Terraform manages the secret container, never its value.** A secret version
  in Terraform is a secret in the state file.
- **The bucket and dataset survive `terraform destroy`.** The tick logs are the
  deliverable; `force_destroy` stays false.

None of it has been applied against a real project yet.

---

## Phases

Do not skip ahead. Each phase gates the next.

| Phase | `-mode` | State | Exit criteria |
|---|---|---|---|
| 1 | `observe` | ✅ stream verified, state renders against live data | An hour of live running with no gaps — **still to do**: only minutes have been run so far |
| 2 | `shadow` | 🔨 code complete, not yet run for real | Several days of clean tick logs, no trading |
| 3 | — | 🔨 tooling built (`cmd/fill`, `cmd/calib`), no real data yet | Calibration analysis run; question set revised on the evidence |
| 4 | `paper` | ⬜ not started, `-mode paper` exits with an error | Fill simulator with realistic maker/taker and queue assumptions |
| 5 | `live` | ⬜ blocked, `-mode live` exits with an error | Minimum size only, after daily loss cap + flatten-on-death exist |

**Phase 2 is where the value is.** It requires no execution code at all and
answers the interesting question. Resist building the executor early.

---

## Immediate TODO

Done since the skeleton:

- ~~Verify the bitbank stream contract against live data~~ →
  [docs/stream-verification.md](docs/stream-verification.md), and `cmd/dump` to
  re-check it.
- ~~`go.sum` / dependency fetch~~ → `gorilla/websocket` only, as expected.
- ~~Sequence handling on the depth book~~ → buffer-and-replay, see above. It is
  not gap detection; the ids are not consecutive.
- ~~Unit tests for `internal/decide`~~ → every gate, table-driven, plus the
  ordering property that brakes beat accelerators.
- ~~Unit tests for `internal/marketstate`~~ → sequencing, bar continuity,
  indicator maths, and a golden test on the state text.
- ~~Forward-fill tool~~ → `cmd/fill`.
- ~~Calibration analysis~~ → `cmd/calib`.

- ~~Somewhere to actually run it~~ → `terraform/`, `deploy/`, and
  `cmd/logcheck` for the hourly verdict. Not yet applied to a real project.

Next, in order:

1. **Run phase 1 for an hour and read the state text.** The renderer has been
   checked against live data for minutes, not hours. Watch for: reconnect
   behaviour, the book going unsynced, a pair going quiet for long enough that
   the carried-forward series says something silly.
2. **Run phase 2 for several days.** This is the whole point. It needs no
   execution code. Pin `-model` to a version and leave it alone for the run.
3. **Then, and only then, phase 3.** `cmd/fill` and `cmd/calib` are written but
   have only ever seen synthetic input. Expect to find that some question has no
   usable ground truth and needs rethinking — that is the phase 3 deliverable.
4. **`internal/exec/paper.go`** — phase 4. Do not start it early.

## Things deliberately not built

- **Backtester.** See rule 1.
- **Live order placement.** Phase 5; `-mode live` exits with an error.
- **Margin/leverage.** Spot only.
- **Multi-pair.** One pair per process. Run more processes if needed.

---

## Conventions

- Go 1.23, standard library where possible. `log/slog` for logging, JSON handler.
- Config via flags and environment. Secrets **only** from environment
  (`TYPESAFE_API_KEY`, and later `BITBANK_API_KEY` / `BITBANK_API_SECRET`).
  Never commit a key, never log one, never write one into a JSONL record.
- No panics in the tick loop. Reconnect and continue; a trading process that
  exits on a network blip is worse than one that retries.
- Numbers in logs and state text are in **bps** or **percent**, labelled. Never
  emit a bare ratio.
- Times are UTC in logs and records; the state text also uses UTC.
- `make check` (vet, gofmt, `go test -race ./...`) must pass before a commit.
  `internal/decide`, `internal/marketstate` and `internal/calib` are pure and
  have no excuse for being untested.
