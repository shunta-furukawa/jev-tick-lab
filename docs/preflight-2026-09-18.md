# First contact with the TypeSafe API — 2026-09-18

Two `cmd/preflight` calls, the first real traffic this repository has ever sent.
Run from a laptop in Japan against `api.typesafe.ai`, which lives in AWS
us-west-2.

## The contract holds

All nine questions answered, answer shapes as documented, the pinned version
answering, `jev.Verify` reporting no problems and no notes. That retires the
largest unknown in the project.

## Measurements

| | call 1 (cold start) | call 2 (60s of history) |
|---|---|---|
| state sent | 706 bytes | 1,330 bytes |
| input tokens | 1,343 | **1,926** |
| output tokens | 228 | 229 |
| latency | 535ms | 561ms |

**Latency has room.** 561ms against a 3s cadence, a 3s call timeout and a 2s
`MaxDecisionAge`. Measured from Japan; the VM in us-west1 sits beside the API,
so expect less. Re-measure there before touching `MaxDecisionAge`.

**The batch is not re-tokenised per question.** 1,926 tokens covers nine
questions against a 3,863-byte request. The vendor's "12.2x cheaper than asking
one at a time" holds, and adding questions stays nearly free.

**Cost, measured:** $0.000081 per call, so **$2.33/day at 3s** and **$11.65 for
a five-day run**. The earlier $1.81 was an estimate against an assumed 1,500
tokens; the real state is bigger than that.

### Where the tokens go

The state text is 1,308 bytes, and **598 of them — 46% — are the "last 60
one-second closes" line**. Prices tokenise badly: `207.5240` is eight characters
and several tokens. Between the two calls, 624 extra bytes of state cost 583
extra tokens, which is close to one token per byte.

Cheaper renderings of the same 60 points, measured:

| | bytes | |
|---|---|---|
| `%.4f` (current) | 598 | `207.5050, 207.5060, ...` |
| `%.3f` | 538 | the fourth decimal is always zero at this tick size |
| bps from the first close | 238 | `+0, +1, +1, ...` — loses resolution at 0 decimals |

Not changed. The state text is the single biggest lever on answer quality, and
trading 46% of it for about a dollar a week is a judgement call for the owner,
not a tidy-up. Worth revisiting if the cadence ever goes back to 1s, where the
same line costs three times as much per day.

## The finding that matters

`anomalous` came back **0.54** on the cold state and **0.41** after 60s of
history, against a `MaxAnomalyNoul` of **0.30**.

The first reading was the renderer's fault and is fixed: a one-bar series was
rendered as "Return 300s: +0.000%, 5m high == 5m low, volatility 0.0 bps", which
describes a halted venue. Windows longer than the series now say what they are
missing instead.

The second reading is not obviously anyone's fault. The market was ordinary —
0.048 bps spread, calm, mildly downward. Two samples is not enough to know
whether Jev simply reads this market as somewhat unusual, or whether 60s of
history is still too little (`Return 300s` and the 5m range were still "n/a"),
or whether 0.30 is the wrong gate.

**Do not tune the threshold on two samples.** It is not necessary, because of
the property below.

## Why a mis-tuned gate costs nothing during phase 2

Shadow mode consumes nothing. `decide.Compose` runs, its `Signal` is written to
the record, and no executor reads it. `Compose` is pure, and every input it
reads is in the record — which `TestSignalIsRederivableFromALoggedRecord` now
enforces, including `Position`, which was missing and has been added.

So the gates are a **derived column**. A collection taken at
`MaxAnomalyNoul = 0.30` can be re-scored at 0.60 months later from the same
JSONL, and the test proves the re-derivation is exact. Choosing thresholds on
evidence is what phase 3 is for; starting the run does not require getting them
right first.

The one thing this does not give you is a meaningful live `Signal` column to
watch during the run. If that matters, raise the gate — but as an operational
convenience, not as a tuning decision.

## Other answers, recorded as a prior for phase 3

From call 2, on a calm mildly-downward xrp_jpy:

| question | answer | |
|---|---|---|
| `regime` | downtrend, p=0.45, conf=0.31 | low confidence; the escape option exists and was not needed |
| `trader_action` | wait, p=0.83, conf=0.78 | clears both floors |
| `momentum` | 1.00, conf=0.80 | mildly downward |
| `volatility` | 0.18, conf=0.85 | very calm — consistent with the 0.048 bps spread |
| `entry_quality` | 1.45, conf=0.43 | below the 3.0 floor |
| `fakeout_risk` | 0.58 | close to its 0.60 gate |
| `book_pressure` | 0.05 | strongly ask-heavy |
| `hold_risk` | 0.24, conf=0.80 | low, and we are flat |

A coherent read of a quiet market. `fakeout_risk` sitting just under its gate is
worth watching in the real dataset; on two samples it means nothing.
