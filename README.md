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

# Phase 2 — Jev evaluation and full logging. Still no trading.
export TYPESAFE_API_KEY=...
go run ./cmd/bot -pair xrp_jpy -mode shadow -model jev-1.13.0 -log-dir ./data
```

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

At roughly 1,500 input tokens per call and $0.042 per million input tokens, one
second of evaluation costs about $0.000063. Output tokens are not billed. So
continuous collection is **$5.44/day**, and eight hours a day is under $2.

The VM it runs on is **$0.44/day** — an e2-micro in `asia-northeast1` with a
20GB disk and an external IP, at September 2026 list prices. That is about 8% of
the total: the length of the run is what costs money, not the machine.

A five-day phase 2 collection is therefore around **$29** all in. Phase 1 makes
no model calls at all, so `-mode observe` costs only the VM.

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

`terraform/` builds an e2-small in `asia-northeast1` (bitbank is domestic; keep
the hop short) inside a VPC whose only ingress is SSH through IAP, plus the log
bucket, a BigQuery dataset and an empty Secret Manager container.
`deploy/deploy.sh` puts the binaries and the systemd units on it — and refuses
to ship a build that does not pass `make check`.

The key is fetched from Secret Manager at start-up into the process environment
and never written to disk. Logs ship to GCS hourly.

**None of this has been applied to a real project yet.** The full runbook,
including what to do when the health check goes red, is in
[docs/operations.md](docs/operations.md).

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
terraform/            the VM and its surroundings
deploy/               systemd units, secret fetch, log shipping, deploy script
docs/                 stream verification record, operations runbook
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
