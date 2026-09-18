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
Language: Go. Deployment: single always-on VM.

**Two owner decisions override the obvious defaults for the initial experiment.
Both are cost decisions and both are reversible.** See "Owner decisions" below
before changing the region or the cadence.

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
| `cmd/preflight` | One real call to TypeSafe, then checks the whole response. Run it before any long collection. `-dry-run` needs no key |

### Key design decisions and why

**Jev calls are asynchronous with at most one in flight.** A slow call must skip
its tick rather than queue, because a queued answer describes a market that no
longer exists. See `inFlight` in `cmd/bot/main.go`.

**Nothing is evaluated until the state is worth reading.** `-min-history`
(default 60s) holds the tick loop until the bar series is long enough that the
state text is not mostly "still building". Two reasons, and the second is the
one that cost real money to learn: a nearly-empty state is not worth paying a
model to read, and — before the renderer was fixed — it actively lied. A series
one bar long rendered as "Return 300s: +0.000%, 5m high == 5m low, volatility
0.0 bps", which describes a halted venue. Preflight scored a perfectly healthy
xrp_jpy book at `anomalous` 0.54 against a 0.30 gate for exactly that reason.
Windows longer than the series now say how much history they are missing.

Measured latency in a live shadow run is **p50 232ms, p99 748ms** against a 3s
cadence — eight times the headroom `MaxDecisionAge` (2s) needs. `cmd/preflight`
reports a higher number, around 550ms, because it makes one cold call including
the TLS handshake while the bot reuses its connection. Treat preflight's figure
as the pessimistic bound it is.

Had latency exceeded `MaxDecisionAge` the run would have collected answers and
produced no decisions at all — every record gated `decision_age`.

**Decision freshness is bounded.** `Thresholds.MaxDecisionAge` (2s). Anything
older is discarded rather than acted on.

**Brakes are evaluated before accelerators.** `decide.Compose` checks the
anomaly gate and the hold-risk gate before any entry logic can run. This
ordering is deliberate — Jev's role in this system is primarily a brake, because
its calibration on financial judgement is exactly what is unproven.

**The gates are a derived column, not a measurement.** Shadow mode consumes
nothing: `Compose` runs, its `Signal` is logged, and no executor reads it. Since
`Compose` is pure and every input it reads is in the record — enforced by
`TestSignalIsRederivableFromALoggedRecord` — a collection taken at one threshold
set can be re-scored at another months later, exactly.

This matters right now. Preflight measured `anomalous` at 0.41 against a 0.30
gate on an ordinary market, so the live `Signal` column may be almost entirely
`gate=anomaly`. **Do not tune on two samples.** Collect, then choose thresholds
on the evidence; that is what phase 3 is for. Raise the gate only if you want a
readable live signal during the run, and treat that as an operational
convenience rather than a tuning decision.

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
to `runs-YYYY-MM-DD.jsonl` with its thresholds, question set and hash, **and the
VCS revision it was built from**, so a tick recorded months ago is still
interpretable.

The revision is there because a restart onto different code is otherwise
invisible. The question-set hash catches a changed question; nothing caught a
changed renderer, and the state text is the biggest lever on answer quality
there is. `go build` stamps the revision automatically; `go run` does not, so a
local run records `unknown (go run)` rather than an empty field that could be
mistaken for a missing one.

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

**Measured 2026-09-18** by `cmd/preflight`, against a production-shaped state
([full record](docs/preflight-2026-09-18.md)):

| | |
|---|---|
| latency, one-shot | **561ms** — a cold call, TLS handshake included |
| latency, steady state | **p50 232ms, p99 748ms** — measured in a live shadow run, where the HTTP connection is reused |
| tokens | **1,926 in, 229 out** for a 1,330-byte state |
| cost | **$0.000081/call — $2.33/day at 3s** |

The batch is **not** re-tokenised per question: 1,926 tokens covers nine
questions against a 3,863-byte request, so the vendor's "12.2x cheaper than
asking one at a time" holds and adding questions stays nearly free. Output
tokens are reported but not billed.

Prices tokenise badly — roughly one token per byte — and **46% of the state
text is the "last 60 one-second closes" line**. Rendering it at `%.3f` or as bps
deltas would cut that substantially. Not done: the state text is the biggest
lever on answer quality, so trading 46% of it for about a dollar a week is the
owner's call.

Latency was measured from Japan to an API in AWS us-west-2. The VM in us-west1
sits beside it, so expect less; re-measure there before touching
`MaxDecisionAge`.

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

Credentials have their own page: [docs/secrets.md](docs/secrets.md) — the whole
inventory is one secret now and one more at phase 5. Two things there are worth
knowing before you touch this area:

- **The only field in a tick record carrying text this code did not construct is
  `error`.** `internal/jev` redacts the credential from every error it returns,
  transport errors included, and `Client` has a `String` method so printing one
  cannot print the key. Records are shipped to GCS and loaded into BigQuery, so
  a leak there would be permanent.
- **The bitbank key at phase 5 must never carry withdrawal permission.** bitbank
  grants 参照 / 取引 / 出金 separately. Nothing in this experiment moves money
  off the exchange.

None of it has been applied against a real project yet.

---

## Owner decisions

Recorded because they contradict what the rest of this document would otherwise
imply, and because they should be revisited at a known point rather than
forgotten.

### Run in the US, not Tokyo — 2026-09-18

`terraform/` defaults to **us-west1** (Oregon). CLAUDE.md's own advice is "keep
the hop short", and this does the opposite for the exchange. Why anyway:

- `e2-micro` is covered by the GCP Always Free tier in us-west1, us-central1 and
  us-east1, and nowhere else. asia-northeast1 is not eligible. Compute and disk
  become free; only the external IP is billed.
- `api.typesafe.ai` resolves into AWS us-west-2, which is also Oregon. The model
  call — which `MaxDecisionAge` gates every signal on — goes from roughly 100ms
  round trip out of Tokyo to roughly 10ms. So this is not purely a downgrade: it
  trades book freshness for model latency, and model latency is the one that can
  silently produce a dataset with no signals in it.
- The cost: the book arrives ~55ms later than it would from Tokyo. At a 3s
  cadence that is under 2% of a tick.

**Revisit before phase 4.** Simulated and real fills are about the round trip to
bitbank, where 110ms is no longer a rounding error.

### Evaluate every 3s, not every 1s — 2026-09-18

The model calls are ~90% of the bill and scale linearly with the cadence, so 1s
to 3s takes a five-day run from about $27 to about $9.

This changes the dataset, not just the price. The premise at the top of this
document is a once-per-second judgement; a 3s series is a coarser one. It is
still the right shape for the question phase 2 and 3 actually ask — is the
confidence calibrated at all — and the forward-fill horizons (10s/60s/300s) all
still land on real records, though +10s resolves to the tick at +12s.

Anything deriving an expected record count from the cadence has to be told:
`deploy/run.sh` passes `-tick`, and the logcheck unit passes the same value. A
3s bot checked against a 1s assumption reports every healthy hour as degraded.

---

## Phases

Do not skip ahead. Each phase gates the next.

| Phase | `-mode` | State | Exit criteria |
|---|---|---|---|
| 1 | `observe` | ✅ **met** — two hours live, 5,217 ticks, zero gaps; 1,319 rendered states checked, which found a permanent `Return 300s` bug ([record](docs/phase1-run.md)) | State text renders correctly against live data for an hour with no gaps |
| 2 | `shadow` | 🔨 code complete, not yet run for real | Several days of clean tick logs, no trading |
| 3 | — | 🔨 tooling built (`cmd/fill`, `cmd/calib`), no real data yet | Calibration analysis run; question set revised on the evidence |
| 4 | `paper` | ⬜ not started, `-mode paper` exits with an error | Fill simulator with realistic maker/taker and queue assumptions |
| 5 | `live` | ⬜ blocked, `-mode live` exits with an error | Minimum size only, after daily loss cap + flatten-on-death exist |

**Phase 2 is where the value is.** It requires no execution code at all and
answers the interesting question. Resist building the executor early.

---

## Where this actually stands

The honest version, because several things in this repository look finished and
are not. **Verified** means it was exercised against the real thing. **Assumed**
means it typechecks, has tests against a fake, and has never met production.

| | |
|---|---|
| bitbank stream contract | ✅ verified live, frames captured ([record](docs/stream-verification.md)) |
| depth sequencing | ✅ unit tested against bitbank's own worked example; 65 min live with zero unsynced ticks |
| indicator maths, state text format | ✅ unit tested, golden test on the rendering |
| no gaps over an hour | ✅ 3,898 ticks, zero gaps, zero reconnects ([record](docs/phase1-run.md)) |
| state text read against live data for an hour | ✅ 1,319 states checked structurally and by eye; found the `Return 300s` bug |
| systemd units | ⚠️ `systemd-analyze verify` passes; never started on a real VM |
| TypeSafe accepts this question set | ✅ **called 2026-09-18.** All nine answered, shapes as documented, pinned version answered |
| model latency | ✅ **561ms**, two samples from Japan. Inside `MaxDecisionAge` |
| input tokens, so the cost figures | ✅ **1,926** measured against a production-shaped state |
| thresholds are re-derivable offline | ✅ tested — a logged record re-scores exactly under any threshold set |
| `decide` against real answers | ⚠️ two samples. Both would trip the anomaly gate — see below |
| `cmd/fill`, `cmd/calib` | ❌ synthetic input only |
| Terraform | ❌ never applied to a project |
| `deploy/deploy.sh` | ❌ syntax checked only |

The bolded rows used to be the unknowns; one `cmd/preflight` call settled them
on 2026-09-18, and found a real defect doing it — see `-min-history` above.
That is what the step is for.

## Next, in order

Ordered by what it costs to find out you are wrong. Everything up to step 2 runs
on a laptop and costs under a dollar; only then is it worth building a VM.

1. ~~`make preflight`~~ — **done 2026-09-18.** The contract holds, latency is
   535ms, and the token count is measured. It also exposed the cold-start state
   defect that `-min-history` and the renderer now fix. Worth one more call from
   the VM once it exists, to measure latency on the path that will actually run.

2. **A short local shadow run.** Half an hour of
   `go run ./cmd/bot -mode shadow -tick 3s -log-dir ./data`, then
   `make logcheck -- -tick 3s`. About $0.04. This is the first time the record schema, the
   gates, the logger and the health check meet real answers, and it is much
   cheaper to find a problem here than on a VM three days in.

3. **`terraform apply`, then seed the secret.** See
   [docs/operations.md](docs/operations.md). Nothing here has been applied to a
   real project, so budget for fixing something on the first attempt.

4. **Deploy and run phase 2 for several days.** The whole point, and it needs no
   execution code. Pin `-model` and leave it alone. Watch the hourly logcheck
   verdict rather than the tick log.

5. **Phase 3.** Expect to find that some question has no usable ground truth.
   Start with `book_pressure`: phase 1 measured the book as bid-heavy 70% of the
   time, so its base rate may be structural rather than informative.

6. **`internal/exec/paper.go`** — phase 4, and the point at which the region
   decision above needs revisiting. Do not start it early.

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
