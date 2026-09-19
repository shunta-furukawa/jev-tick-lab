# jev-tick-lab

A forward-only experiment: can [Jev](https://typesafe.ai) — TypeSafe's System
One decision model — reproduce the snap judgements of a short-term day trader at
a one-second cadence, and **is its confidence calibrated on a task it was never
trained for?**

This repository runs the experiment on bitbank spot markets, logs every
evaluation, and produces the data to answer that question.

**It is not a profitable trading system and is not investment advice.** The
deliverable is a dataset and a calibration curve. A run that loses money but
produces a clean curve is a success; a run that makes money with no logged
confidence data is a failure.

See [CLAUDE.md](./CLAUDE.md) for the architecture and the rules that govern
changes to this code — in particular, why there is no backtest and never will
be one.

## Quick start

```bash
go mod download

# Phase 1 — stream and state only, no model calls, no API key needed.
go run ./cmd/bot -pair xrp_jpy -mode observe -print-state

# Before any long run: one real call, checked end to end.
export TYPESAFE_API_KEY=...
go run ./cmd/preflight -pair xrp_jpy -model jev-1.13.0

# Phase 2 — Jev evaluation and full logging. Still no trading.
go run ./cmd/bot -pair xrp_jpy -mode shadow -model jev-1.13.0 -tick 3s -log-dir ./data
```

`cmd/preflight` exists because nothing else here has ever talked to the real
API — the client is tested against a fake, and `jev.Validate` only checks the
question set locally. A 422 on the first tick of a multi-day run would produce
days of records containing nothing but an error string. One call settles it, and
prints the measured tokens and latency that every cost figure below assumes.

Records land in `./data/ticks-YYYY-MM-DD.jsonl`, one JSON object per tick, plus
one row per process start in `./data/runs-YYYY-MM-DD.jsonl` holding the
thresholds and question set that produced them.

`-mode paper` and `-mode live` exit with an error. They are phases 4 and 5, and
the phases gate each other.

## Phase 3: what the logs are for

```bash
# Join every tick to the price 10s / 60s / 300s later.
go run ./cmd/fill -in data/ticks-2026-09-17.jsonl

# Bucket the model's stated probability, and see how often it was right.
go run ./cmd/calib -in data/ticks-2026-09-17-filled.jsonl
go run ./cmd/calib -in data/ticks-2026-09-17-filled.jsonl \
  -question book_pressure -outcome direction -horizon 60 -csv reliability.csv
```

`cmd/calib` prints how often each brake fired, the latency and token profile of
the run, and the reliability table: stated probability against realised
frequency, with the gap between them. A positive gap is overconfidence.

The `-band-bps` flag is the honest version of the question. With `-band-bps 0` a
directional call is right if the price moved the right way at all; with
`-band-bps 24` it is right only if the move covered an all-taker round trip on a
JPY alt. The difference between those two numbers is the whole fee problem.

## Checking the exchange contract

```bash
go run ./cmd/dump -pair xrp_jpy -for 20s
```

Prints raw stream frames without going through `internal/stream`, so it shows
the wire rather than our reading of it. The current contract was verified this
way — see [docs/stream-verification.md](docs/stream-verification.md).

## Cost

Measured: **1,926 input tokens** for the nine-question batch, at $0.042 per
million, output reported but not billed. The initial experiment runs at a **3s**
cadence, which is **$2.33/day**; at 1s it would be $7.00.

The VM it runs on is **$0.11/day**: an `e2-micro` with a 30GB standard disk in
`us-west1`, a configuration chosen to sit inside the GCP Always Free tier, so
only the external IP and a little egress are billed. That is under 6% of the
total — cadence and run length are the only levers that matter.

A five-day phase 2 collection is therefore around **$12** all in. Phase 1
makes no model calls at all, so `-mode observe` costs only the VM.

Full breakdown, including how to stop paying between runs, in
[docs/operations.md](docs/operations.md#cost).

## Running it somewhere

A persistent WebSocket connection with in-memory state is a poor fit for
anything serverless. It wants a small always-on VM, and phase 2 wants several
days of uninterrupted collection from it.

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars   # fill in project_id
terraform apply

printf %s "$TYPESAFE_API_KEY" | \
  gcloud secrets versions add jev-tick-lab-typesafe-api-key --data-file=-

./deploy/deploy.sh YOUR_PROJECT
```

`terraform/` builds a free-tier `e2-micro` in `us-west1` inside a VPC whose only
ingress is SSH through IAP, plus the log bucket, a BigQuery dataset and an empty
Secret Manager container. Oregon rather than Tokyo is a deliberate cost decision
— and it also puts the VM in the same region as the model API. The reasoning,
and when to revisit it, is in CLAUDE.md under "Owner decisions".
`deploy/deploy.sh` puts the binaries and the systemd units on it — and refuses
to ship a build that does not pass `make check`.

The key is fetched from Secret Manager at start-up into the process environment
and never written to disk. Logs ship to GCS hourly.

**None of this has been applied to a real project yet.** The full runbook,
including what to do when the health check goes red, is in
[docs/operations.md](docs/operations.md).

### Watching it

```bash
make watch      # collect and serve the dashboard, one command
```

Then open <http://127.0.0.1:8080>. The page rebuilds as the log grows and says
how stale it is, so a stopped collector is visible without reading a log file.
`make serve` gives the same dashboard against data you already have.

### Taking it with you

```bash
go run ./cmd/report -in data/ticks-2026-09-18-filled.jsonl -tick 3s
```

One self-contained HTML file: collection health over time, what stopped each
tick, each question's answers drawn against the gate that reads them, and — on a
filled log — the reliability curve the experiment exists to produce. No CDN, no
fonts, no external scripts, so it opens the same from a laptop, a GCS bucket, or
an archive in a year.

### Knowing whether the run is still worth anything

```bash
go run ./cmd/logcheck -dir ./data -window 1h
```

systemd restarts a dead bot. It cannot see a bot that is alive, writing a record
every second, and writing records nobody can analyse — because the book never
re-synced after a reconnect, or because the pinned model version moved halfway
through the run. `logcheck` runs hourly on the VM and fails the unit on exactly
that, so it surfaces in Cloud Logging instead of in phase 3, a week too late.

## Layout

```
cmd/bot/              entrypoint, tick loop
cmd/dump/             raw stream frames, for verifying the exchange contract
cmd/fill/             forward-fill: joins each tick to its future price
cmd/calib/            the calibration report
internal/stream/      bitbank public stream (Socket.IO v4)
internal/marketstate/ order book, 1s bars, indicators, state rendering
internal/jev/         TypeSafe client and the question set
internal/decide/      thresholds and signal composition — all weights live here
internal/obs/         JSONL tick logger
internal/calib/       reliability bins, Brier, ECE
internal/health/      is the collection still producing usable data?
internal/exec/        fill simulation (phase 4, not implemented)
cmd/logcheck/         hourly health verdict, exit code is the interface
cmd/preflight/        one real API call, fully checked, before a long run
cmd/report/           a self-contained HTML page of a run
cmd/serve/            the same page, live over HTTP
internal/report/      its summaries and SVG
terraform/            the VM and its surroundings
deploy/               systemd units, secret fetch, log shipping, deploy script
docs/                 stream verification, operations runbook, secrets inventory
```

## Development

```bash
make check   # vet, gofmt check, go test -race ./...
```

## Status

**Phase 1 passed**: 65 minutes against the live exchange, 3,898 ticks, zero gaps,
zero reconnects — [the record](docs/phase1-run.md), including the three things
worth carrying into phase 2.

Phase 2 is code complete and has infrastructure to run on, but has not been run
long enough to produce a dataset. The phase 3 tools are written and tested
against synthetic input only. See the TODO list in [CLAUDE.md](./CLAUDE.md).
