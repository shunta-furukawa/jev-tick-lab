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
second of evaluation costs about $0.000063. Eight hours a day is **under
$2/day**. Output tokens are not billed.

## Infrastructure

A persistent WebSocket connection with in-memory state is a poor fit for
anything serverless. Use a small always-on VM.

```
GCE e2-micro or e2-small, asia-northeast1  (bitbank is domestic; keep the hop short)
├── systemd unit with Restart=always         → deploy/jev-tick-lab.service
├── TYPESAFE_API_KEY from Secret Manager     → fetched at start, never on disk
├── JSONL logs to local disk                 → daily sync to GCS
└── GCS → BigQuery for the calibration analysis
```

At one record per second a full day is about 29,000 rows, so a nightly batch
load is plenty — no streaming inserts needed.

### Deploy

```bash
make build-linux
gcloud compute scp bin/bot jevbot-vm:~/ --zone asia-northeast1-b
gcloud compute scp deploy/jev-tick-lab.service jevbot-vm:~/ --zone asia-northeast1-b

# on the VM
sudo mv jev-tick-lab.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now jev-tick-lab
journalctl -u jev-tick-lab -f
```

### Log shipping

```bash
# cron, daily
gsutil -m rsync -r /opt/jev-tick-lab/data gs://YOUR_BUCKET/jev-tick-lab/
bq load --source_format=NEWLINE_DELIMITED_JSON --autodetect \
  jevbot.ticks gs://YOUR_BUCKET/jev-tick-lab/ticks-*.jsonl
```

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
internal/exec/        fill simulation (phase 4, not implemented)
deploy/               systemd unit
docs/                 stream verification record
```

## Development

```bash
make check   # vet, gofmt check, go test -race ./...
```

## Status

Phase 1 is verified against the live exchange; phase 2 is code complete but has
not yet been run for long enough to produce a dataset. The phase 3 tools are
written and tested, but have only ever seen synthetic input. See the TODO list
in [CLAUDE.md](./CLAUDE.md).
