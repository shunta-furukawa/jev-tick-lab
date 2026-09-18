# jevbot

Can [Jev](https://typesafe.ai) — TypeSafe's System One decision model —
reproduce the snap judgements of a short-term day trader at a one-second
cadence, and is its confidence calibrated on a task it was never trained for?

This repository runs the experiment on bitbank spot markets, logs every
evaluation, and produces the data to answer that question.

**It is not a profitable trading system and is not investment advice.**

See [CLAUDE.md](./CLAUDE.md) for the architecture and the rules that govern
changes to this code.

## Quick start

```bash
go mod tidy

# Phase 1 — stream and state only, no model calls, no API key needed.
go run ./cmd/bot -pair xrp_jpy -mode observe

# Phase 2 — Jev evaluation and full logging. Still no trading.
export TYPESAFE_API_KEY=...
go run ./cmd/bot -pair xrp_jpy -mode shadow -model jev-1.13.0 -log-dir ./data
```

Records land in `./data/ticks-YYYY-MM-DD.jsonl`, one JSON object per tick.

## Cost

At roughly 1,500 input tokens per call and $0.042 per million input tokens,
one second of evaluation costs about $0.000063. Eight hours a day is
**under $2/day**. Output tokens are not billed.

## Infrastructure

A persistent WebSocket connection with in-memory state is a poor fit for
anything serverless. Use a small always-on VM.

```
GCE e2-micro or e2-small, asia-northeast1  (bitbank is domestic; keep the hop short)
├── systemd unit with Restart=always         → deploy/jevbot.service
├── TYPESAFE_API_KEY from Secret Manager     → fetched at start, never on disk
├── JSONL logs to local disk                 → daily sync to GCS
└── GCS → BigQuery for the calibration analysis
```

At one record per second a full day is about 29,000 rows, so a nightly batch
load is plenty — no streaming inserts needed.

### Deploy

```bash
GOOS=linux GOARCH=amd64 go build -o bin/bot ./cmd/bot
gcloud compute scp bin/bot jevbot-vm:~/ --zone asia-northeast1-b
gcloud compute scp deploy/jevbot.service jevbot-vm:~/ --zone asia-northeast1-b

# on the VM
sudo mv jevbot.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now jevbot
journalctl -u jevbot -f
```

### Log shipping

```bash
# cron, daily
gsutil -m rsync -r /opt/jevbot/data gs://YOUR_BUCKET/jevbot/
bq load --source_format=NEWLINE_DELIMITED_JSON --autodetect \
  jevbot.ticks gs://YOUR_BUCKET/jevbot/ticks-*.jsonl
```

## Layout

```
cmd/bot/              entrypoint, tick loop
internal/stream/      bitbank public stream (Socket.IO v4)
internal/marketstate/ order book, 1s bars, indicators, state rendering
internal/jev/         TypeSafe client and the question set
internal/decide/      thresholds and signal composition — all weights live here
internal/obs/         JSONL tick logger
internal/exec/        fill simulation (not yet implemented)
deploy/               systemd unit
```

## Status

Skeleton. The bitbank stream contract has **not** been verified against a live
connection yet — that is the first task. See the TODO list in CLAUDE.md.
