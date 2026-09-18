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
                                        │
                              every 1s  │  Render() → state text
                                        ▼
                                   jev.Ask()  (async, max 1 in flight)
                                        │
                                        ▼
                                 decide.Compose()  ← all weights live here
                                        │
                        ┌───────────────┴───────────────┐
                        ▼                               ▼
                 obs.Logger (JSONL)              executor (paper/live)
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

**Everything is logged, including failures.** A failed call writes a record with
`error` set. Gaps in the log are themselves data.

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

⚠️ The exact room names and payload field names in `internal/stream` and
`internal/marketstate` were written from secondary sources and **have not been
verified against a live connection**. Verify against
<https://github.com/bitbankinc/bitbank-api-docs> before relying on them, and fix
them in place if they differ.

**Depth handling**: `depth_whole` is a full snapshot, `depth_diff` is
incremental with amount `0` meaning delete. There is a `sequenceId` on both.
Gap detection is currently **not implemented** — see TODO below.

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

## Phases

Do not skip ahead. Each phase gates the next.

| Phase | `-mode` | State | Exit criteria |
|---|---|---|---|
| 1 | `observe` | 🔨 skeleton exists, unverified | State text renders correctly against live data for an hour with no gaps |
| 2 | `shadow` | 🔨 skeleton exists | Several days of clean tick logs, no trading |
| 3 | — | ⬜ not started | Calibration analysis run; question set revised on the evidence |
| 4 | `paper` | ⬜ not started | Fill simulator with realistic maker/taker and queue assumptions |
| 5 | `live` | ⬜ blocked | Minimum size only, after daily loss cap + flatten-on-death exist |

**Phase 2 is where the value is.** It requires no execution code at all and
answers the interesting question. Resist building the executor early.

---

## Immediate TODO

1. **Verify the bitbank stream contract against live data.** Connect, dump raw
   frames, confirm room names and field names. Fix `internal/stream` and
   `internal/marketstate` where they differ. Nothing else is trustworthy until
   this is done.
2. **`go.sum` / dependency fetch.** Only `gorilla/websocket` is required so far.
3. **Sequence-gap detection on the depth book.** Track `sequenceId`; on a gap,
   discard the book and wait for the next `depth_whole`. Currently diffs are
   applied blindly, so the book can silently drift.
4. **Unit tests for `internal/decide`.** It is pure — table-driven tests over
   answer fixtures covering each gate. This is the highest-value test in the repo.
5. **Unit tests for `internal/marketstate`** indicator maths against fixtures.
6. **Forward-fill tool** (`cmd/fill`): read a JSONL day, join each record to the
   price 10s/60s/300s later, write the enriched file. Needed for phase 3.
7. **Calibration analysis** (`cmd/calib` or a notebook): bucket by `confidence`,
   plot realised accuracy per bucket. This is the headline chart of the writeup.
8. **`internal/exec/paper.go`** — does not exist yet. Phase 4.

---

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
