# Phase 1 — an hour against the live exchange

CLAUDE.md's phase 1 exit criterion: *the state text renders correctly against
live data for an hour with no gaps.*

**Run:** `xrp_jpy`, `-mode observe`, 2026-09-18 00:30:56 – 01:35:56 UTC, 65
minutes, 1s cadence. Raw journal: `docs/evidence/phase1-observe-2026-09-18.log.gz`.

Not from the production VM — this ran from a development container, so the
network path is not the one a deployed run would take. What it exercises is the
stream client, the book maintenance and the derived numbers, which is what the
criterion is about.

## Result: the no-gaps half, yes. The renders-correctly half, not yet.

The criterion has two parts and this run only settles one of them. Recorded
plainly because the phase table briefly claimed otherwise: the exit criterion
was paraphrased as "an hour of live running with no gaps", which quietly dropped
"state text renders correctly", and then marked as passed. The original wording
is restored.

### No gaps: met

| | |
|---|---|
| snapshots | 3,898 in 3,897 seconds |
| gaps > 2s | **0** |
| reconnects | **0** |
| warmup | 2.0s from start to `warmed up` |
| ticks with no price (`last == 0`) | 0 |
| `circuit_break` | `NONE` for all 3,898 |

One `not ready` warning, one second after start, before the rooms were joined —
which is the warmup path doing exactly what it should.

### Renders correctly: not met

This run logged snapshot fields rather than rendered text, to keep the journal
machine-readable. What exists instead: `TestRenderGolden` pins the format, the
text was read against live data by eye for a few minutes, and none of the
underlying fields went nonsensical over the hour (no tick without a price, all
spreads and imbalances in range, `circuit_break` NONE throughout).

That is good evidence and it is not the criterion. Finishing it costs an hour
and no API key:

```bash
go run ./cmd/bot -pair xrp_jpy -mode observe -print-state
```

## What the hour showed about the market

| | |
|---|---|
| price | 201.557 – 203.275 (85 bps range) |
| spread | median **0.049 bps** (one tick), max 5.08 bps |
| depth imbalance (bid share) | p25 0.40, **p50 0.685**, p75 0.85 |
| prints in the trailing 30s | median 3, max 48 |
| **ticks where nothing had traded for 30s** | **1,107 (28.4%)** |

### Three things worth carrying into phase 2

**1. The sparse-trade problem is real, and bigger than the sample that found
it.** Over a quarter of the hour, xrp_jpy had not printed a trade in 30 seconds.
The original per-print bar series would have mislabelled every one of those
ticks: "the last 60 one-second closes" would have reached back minutes, and
"Return 60s" would have been a return over the last 60 trades. This is the
single most load-bearing fix in `internal/marketstate`, and an hour of live data
says it was needed.

**2. The book is persistently bid-heavy.** Median imbalance is 0.685, not 0.5 —
the top ten levels lean toward bids about 70% of the time, structurally. That is
a calibration problem waiting in phase 3: `book_pressure` asks whether the book
leans toward buyers, and a question whose true answer is "yes" 70% of the time
from resting-order structure alone will look well-calibrated while carrying
almost no information. Worth checking its base rate before trusting its
reliability curve. Do not rename it — the id is schema (rule 6).

**3. `MaxSpreadBps` = 25 never came close to firing.** The widest spread all
hour was 5.08 bps, against a median of 0.049. The gate is a disaster brake, not
a working filter, which is the right shape for a brake — but nothing in this run
tested it. Leave it until there is evidence, rather than tuning against one
quiet hour.

Three ticks (0.08%) showed a near-one-sided top-ten book (imbalance 0.001), each
lasting one second, each with a normal one-tick spread. That is a thin bid side
for a moment, not a broken book.

## What this run does *not* establish

- **The state text itself.** This run logged the snapshot fields rather than the
  rendered text, so that the journal stayed machine-readable. The rendering was
  checked against live data by eye earlier, and `TestRenderGolden` pins the
  format, but no hour-long visual check was made.
- **Reconnect behaviour.** Zero disconnects in 65 minutes is a good sign and no
  evidence at all about the reconnect path. That code is covered by a fake
  server in `internal/stream`, not by production.
- **A quiet pair going quiet for much longer.** The longest no-print stretch
  here was well under the carried-forward series' 300-bar window.
