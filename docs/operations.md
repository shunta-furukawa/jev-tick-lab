# Operations

How to get a collection run onto a machine and know whether it is still worth
anything. Phase 2 wants several days of clean logs, and "clean" is the hard
part: a bot that is alive and writing useless records looks exactly like a bot
that is working.

> **Status: none of this has been applied yet.** The Terraform has not been run
> against a real project, and the systemd units verify with `systemd-analyze`
> but have not started on a real VM. Expect to fix something the first time.
> What *has* been exercised is `cmd/logcheck`, against real log files.

---

## What runs where

```
terraform/          builds the machine and its surroundings, once
deploy/deploy.sh    puts code on the machine, as often as needed
```

They are deliberately separate. A code deploy must never be able to touch the
bucket holding collected data, and rebuilding the VM must never require a code
change. Terraform owns the substrate; deploy.sh owns the payload.

On the machine:

| unit | what it does | when |
|---|---|---|
| `jev-tick-lab.service` | the collector | always, restarts on failure |
| `jev-tick-lab-ship.timer` | copies tick logs to GCS | hourly |
| `jev-tick-lab-logcheck.timer` | decides whether the data is usable | hourly, at :07 |

---

## One-time setup

### 1. Enable the APIs

Not managed in Terraform on purpose: `terraform destroy` would then disable
APIs that other things in the project may be using.

```bash
gcloud services enable \
  compute.googleapis.com \
  secretmanager.googleapis.com \
  storage.googleapis.com \
  bigquery.googleapis.com \
  iap.googleapis.com \
  logging.googleapis.com \
  monitoring.googleapis.com \
  --project YOUR_PROJECT
```

### 1b. Check your own permissions

There is no public SSH: port 22 is open to Google's IAP forwarding range only.
Reaching the VM therefore needs, on your own account, `roles/iap.tunnelResourceAccessor`
and `roles/compute.osLogin` (project Owner already implies both). Without them
`--tunnel-through-iap` fails with a permission error that reads like a network
problem.

### 2. Apply the Terraform

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars   # fill in project_id
terraform init
terraform plan      # read it; it should create ~12 resources and destroy none
terraform apply
```

Optionally move state to GCS first — see the commented backend block in
`versions.tf`. Local state is fine for one VM right up until the laptop dies.

What it creates: a dedicated VPC whose only ingress is SSH from Google's IAP
range, a service account, the log bucket, a BigQuery dataset, an empty Secret
Manager container, and the VM.

### 3. Seed the API key

Terraform creates the secret *container* and never the value: a secret version
in Terraform is a secret in the state file, which is a secret in a bucket that
more people can read than you think.

```bash
printf %s "$TYPESAFE_API_KEY" | \
  gcloud secrets versions add jev-tick-lab-typesafe-api-key --data-file=-
```

`printf %s` rather than `echo`, so no trailing newline ends up in the key.

### 4. Deploy

```bash
./deploy/deploy.sh YOUR_PROJECT
```

It runs `make check` first and refuses to ship a build that does not pass.
Deploying an untested binary into a multi-day run is how a week of data gets
thrown away. `SKIP_CHECK=1` exists for emergencies.

---

## Where the key lives

Worth being explicit, because the first version of this got it wrong.

The unit reads `/etc/jev-tick-lab/config`, which holds the pair, the model, the
mode and the paths — **no secrets**. `ExecStart` is `run.sh`, which fetches the
key from Secret Manager using the VM's own service-account token, exports it,
and `exec`s the bot in its place.

So the key exists only in the collector process's environment. It is not in an
`EnvironmentFile`, not in the image, not in a disk snapshot, and not in
Terraform state. Rotating it is `gcloud secrets versions add` followed by
`systemctl restart jev-tick-lab`.

`run.sh` uses only `curl` and `python3`, both already on the Debian image, so
start-up never waits for `apt` to finish.

---

## Verifying a run

First minutes:

```bash
gcloud compute ssh jev-tick-lab --zone asia-northeast1-b --tunnel-through-iap \
  -- journalctl -u jev-tick-lab -f
```

Expect, in order: `started`, one `not ready` while the book is unseeded, five
`joined room` lines, then `warmed up`, then `evaluated` once a second.

After an hour, ask the question that matters:

```bash
gcloud compute ssh jev-tick-lab --zone asia-northeast1-b --tunnel-through-iap \
  -- sudo -u jevbot /opt/jev-tick-lab/logcheck -dir /opt/jev-tick-lab/data -window 1h
```

---

## Reading the health check

`logcheck` exits 0 healthy, 1 degraded, 2 could-not-tell. The hourly timer turns
a degraded window into a failed unit and a structured `ERROR` line that Cloud
Logging can alert on.

It checks the data, not the process, because systemd already restarts a dead
bot. What systemd cannot see is a bot that is alive and producing records that
nobody will be able to analyse.

| problem | what it usually means | what to do |
|---|---|---|
| `no records in the last 1h` | the collector is not running, or is writing somewhere else | `systemctl status jev-tick-lab`, then the journal |
| `newest record is Ns old` | it died or wedged partway through the window | as above; check for an OOM kill in `dmesg` |
| `only N of an expected M records` | ticks are being skipped — a slow model, or CPU throttling | check the `latency` numbers; if p99 is near the 3s call timeout, the model is slow, not the VM. If latency is fine, consider a bigger machine type |
| `N% of calls failed` | TypeSafe is refusing or overloaded | the journal has the status. 401 means the key is wrong or rotated; 429 means the rate limit moved; 529 means overload, which is theirs to fix |
| `N% ran against an unseeded book` | reconnects without a `depth_whole` following | if it persists, bitbank changed something: run `cmd/dump` and re-verify the contract |
| `N% ran against a stale feed` | the stream is connected but silent | usually the same cause as above |
| `longest hole in the series is X` | a restart, a reconnect, or the VM was preempted | correlate with `journalctl -u jev-tick-lab --since ...` |
| `the model version changed mid-window` | **stop and think.** The pinned id resolved to a new version, which invalidates every tuned threshold (CLAUDE.md rule 5) | treat the run as two runs. Records either side are not comparable |

A halted market (`circuit break`) is counted but is **not** a failure: those
ticks are still data, and `decide` already stands aside during one.

---

## Getting the data out

Shipping is hourly and automatic. To load a day into BigQuery for analysis:

```bash
gcloud storage ls gs://YOUR_PROJECT-jev-tick-lab-logs/xrp_jpy/

bq load --source_format=NEWLINE_DELIMITED_JSON --autodetect \
  jev_tick_lab.ticks \
  gs://YOUR_PROJECT-jev-tick-lab-logs/xrp_jpy/ticks-2026-09-17.jsonl
```

The VM has **no** BigQuery permissions, deliberately. It collects and ships;
loading runs from a workstation, so a compromised VM cannot rewrite data it has
already delivered.

For the calibration work the files are enough — no BigQuery needed:

```bash
gcloud storage cp gs://.../ticks-2026-09-17.jsonl ./data/
go run ./cmd/fill  -in data/ticks-2026-09-17.jsonl
go run ./cmd/calib -in data/ticks-2026-09-17-filled.jsonl
```

---

## Changing things mid-experiment

Don't, mostly. A run is defined by its model version, its question set and its
thresholds, all of which are recorded in `runs-YYYY-MM-DD.jsonl` at start-up.

- **Pair or model**: edit `terraform.tfvars`, `terraform apply` (rewrites
  `/etc/jev-tick-lab/config` on next boot), then restart. This starts a new run;
  treat the data as a new dataset.
- **Thresholds or questions**: a code change, so a deploy. Same conclusion.
- **Machine type**: `terraform apply` stops and starts the VM. Costs a few
  minutes of ticks.

---

## Cost

Roughly, in `asia-northeast1`, per month:

| item | approx |
|---|---|
| e2-small, always on | $15 |
| 20GB pd-balanced | $2 |
| ephemeral external IP | $3 |
| GCS standard, a few GB | under $1 |
| Jev calls, 8h/day | under $60 |

The external IP is there for egress only. Cloud NAT would be the alternative and
costs several times more than the VM it protects, with nothing listening on this
host either way.

e2-micro halves the compute line. Check the skipped-tick count in `logcheck`
first — the tick loop is latency-sensitive and e2-micro's 0.25 vCPU baseline can
throttle exactly when a burst of depth diffs arrives.

---

## Teardown

```bash
cd terraform && terraform destroy
```

The bucket and the BigQuery dataset are configured **not** to be destroyed with
contents (`force_destroy = false`, `delete_contents_on_destroy = false`). This
is on purpose: the tick logs are the deliverable of the experiment, and
`terraform destroy` must not be able to throw away a week of collection. Empty
them by hand first if you genuinely mean it.
