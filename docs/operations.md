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

## Where it runs, and why there

The default is **us-west1 (Oregon)**, not `asia-northeast1`, and that is a
deliberate owner decision recorded in CLAUDE.md. Two reasons: `e2-micro` is only
free-tier eligible in us-west1, us-central1 and us-east1, and `api.typesafe.ai`
resolves into AWS us-west-2 — also Oregon — so the model call drops from roughly
a 100ms round trip out of Tokyo to roughly 10ms. That matters more than it
sounds: `decide` gates any answer older than 2s, so model latency is what
decides whether a run produces signals at all.

The cost is that the bitbank book arrives ~55ms later than it would from Tokyo.
At the configured 3s cadence that is under 2% of a tick. **Revisit before phase
4**, where fills depend on the round trip to Tokyo rather than to Oregon.

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

### 4. Pre-flight the API

> **Cheaper still: run this on your laptop before step 2.** It needs a TypeSafe
> key and nothing else — no project, no VM, no Terraform. If the API does not
> accept this question set, or answers more slowly than `MaxDecisionAge`, you
> want to know that before building anything to run it on.

**Do this before any collection longer than a few minutes.** Nothing in this
repository had ever talked to the real TypeSafe API: `internal/jev` is tested
against a local fake, and `jev.Validate` only checks the question set's shape
without sending it.

```bash
export TYPESAFE_API_KEY=...
go run ./cmd/preflight -pair xrp_jpy -model jev-1.13.0
```

One call, about $0.00006. It connects to bitbank, renders a real state, sends it
once, and then checks the response the way no local test can: that the API
accepts this question set, that every question came back, that the answer shapes
match the documentation, that noul answers carry no confidence (rule 4), and
that the model which answered is the one that was pinned (rule 5).

It also prints the **measured** token count and latency. Until it has run, every
cost figure below is an assumption, and so is the claim that a 1s cadence fits
inside the 3s call timeout.

Exit 1 means do not start the run yet. `-dry-run` does everything except the
call and needs no key.

### 5. Deploy

```bash
./deploy/deploy.sh YOUR_PROJECT
```

It runs `make check` first and refuses to ship a build that does not pass.
Deploying an untested binary into a multi-day run is how a week of data gets
thrown away. `SKIP_CHECK=1` exists for emergencies.

---

## Where the key lives

> The full credential inventory, rotation procedure, and the phase 5 bitbank
> permissions are in [secrets.md](secrets.md). What follows is how the one
> secret that exists today reaches the process.


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
gcloud compute ssh jev-tick-lab --zone us-west1-b --tunnel-through-iap \
  -- journalctl -u jev-tick-lab -f
```

Expect, in order: `started`, one `not ready` while the book is unseeded, five
`joined room` lines, then `warmed up`, then `evaluated` once a second.

After an hour, ask the question that matters:

```bash
gcloud compute ssh jev-tick-lab --zone us-west1-b --tunnel-through-iap \
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

## Alerting

Set `alert_email` in `terraform.tfvars` and the apply also creates two policies:

- **collection degraded** — logcheck ran and said the last hour is unusable.
- **no health report** — nothing has reported for three hours. This is the one
  that matters. A log-match alert cannot fire when the problem is that no logs
  arrive, so this watches a log-based counter for absence instead, and catches
  the VM being gone, wedged, or never booted.

The filters are full-text matches rather than `jsonPayload` field lookups,
because the ops agent forwards the journal line as text unless it has been
configured to parse it, and a filter that silently matches nothing is worse than
no alert at all. **Confirm they match after the first hour:**

```bash
gcloud logging read '"\"msg\":\"logcheck\""' --limit 5 --freshness 2h
```

If that returns nothing while logcheck is clearly running, fix the filter in
`terraform/alerts.tf` before trusting the silence.

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

List prices checked September 2026. The default configuration is built to sit
inside the GCP Always Free tier, which is the whole reason it runs in Oregon
rather than Tokyo.

### The machine: about $3/month

| item | | per month |
|---|---|---|
| e2-micro in us-west1 | Always Free | **$0** |
| 30GB pd-standard | Always Free (30 GB-months) | **$0** |
| external IPv4 | $0.004/hr, not covered | **$2.92** |
| GCS standard, ~2.6GB | | **$0.06** |
| egress to the model API | ~3.3GB, 1GB free from N. America | **$0.28** |
| | | **≈ $3.26/month, or $0.11/day** |

What forfeits the free tier, in order of how easily it happens by accident:

- **A second VM.** The allowance is one e2-micro's worth of hours per month,
  pooled across us-west1, us-central1 and us-east1. One pair per process means
  a second pair is a second VM, and it is billed in full.
- **A bigger machine.** Only `e2-micro` qualifies. e2-small is about $12/month.
- **A bigger or faster disk.** Only standard persistent disk, only up to 30GB.
  pd-balanced would be roughly $3/month for the same size.
- The region. asia-northeast1 is not eligible at all; there an identical
  e2-micro is $7.84/month and the disk is $1.56.

### The model calls, which are still the actual bill

**Measured 2026-09-18** against a production-shaped state: 1,926 input tokens
for the nine-question batch, at $0.042 per million, output reported but not
billed. Details in [preflight-2026-09-18.md](preflight-2026-09-18.md).

| cadence | calls/day | per day | per month |
|---|---|---|---|
| **3s (the configured default)** | 28,800 | **$2.33** | $70 |
| 1s (the premise in CLAUDE.md) | 86,400 | $7.00 | $210 |
| 5s | 17,280 | $1.40 | $42 |

46% of the state text is the 60-closes line, so a cheaper rendering of it is the
one lever that would move these numbers without changing the cadence.

The machine is now under 6% of the bill. Cadence and run length are the only
levers that matter.

### What a phase 2 run actually costs

The exit criterion is "several days of clean tick logs", not a month. Five days
at the configured 3s:

```
infrastructure   5 × $0.11  =  $0.55
model calls      5 × $2.33  = $11.65
                              -------
                              ≈ $12.20
```

For reference, the same five days at 1s in asia-northeast1 on an e2-small — the
configuration this started from — would have been about $29.

Phase 1 is free of model calls entirely: `-mode observe` never calls TypeSafe,
so an hour, or a day, of stream verification costs only the VM. So does
`cmd/preflight -dry-run`.

### Between runs

Stop the VM. A stopped instance bills nothing for compute, and an ephemeral
external IP is released when it stops, so the standing cost falls to the disk —
which is inside the free allowance, so effectively zero.

```bash
gcloud compute instances stop jev-tick-lab --zone us-west1-b
gcloud compute instances start jev-tick-lab --zone us-west1-b
```

The collector comes back on boot, re-seeds the book from the first
`depth_whole`, and carries on. It is a new run in every sense that matters, so
it writes a new row to `runs-YYYY-MM-DD.jsonl`.

Note that stopped hours do not bank free-tier hours for a second VM later: the
allowance is per month, not a balance.

### A budget you will actually notice

Nothing here can run away — the cost is flat and predictable. The realistic
failure is forgetting it is on. Set `billing_account` and `alert_email` in
`terraform.tfvars` and the apply creates a budget that mails you at 50%, 90% and
100% of `budget_usd`.

```bash
gcloud billing accounts list    # for the id
```

A budget alerts; it does not stop anything. Stopping is still the `instances
stop` above.

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
