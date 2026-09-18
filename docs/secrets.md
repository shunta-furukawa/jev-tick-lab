# Secrets

Every credential this project needs, when it starts needing it, and what must
never be granted to it.

The short version: **one secret today**, a second one at phase 5, and nothing
else. Most of what looks like it belongs here is configuration, and keeping that
line sharp is the point of this document.

---

## The rule

| | goes in | why |
|---|---|---|
| credentials | Secret Manager, fetched at start-up into the process environment | never on disk, never in an image, never in Terraform state |
| everything else | `/etc/jev-tick-lab/config`, written by the Terraform startup script | it is not sensitive and it should be readable when debugging at 3am |

`/etc/jev-tick-lab/config` holds `JEV_PAIR`, `JEV_MODEL`, `JEV_MODE`,
`JEV_TICK`, `JEV_LOG_DIR`, `JEV_PROJECT`, `JEV_SECRET_ID`, `JEV_GCS_BUCKET`.
Note what the last two are: the *name* of the secret and the *name* of the
bucket. Names are not credentials.

Nothing stops someone adding `TYPESAFE_API_KEY=` to that file — the unit would
read it quite happily. That is the one part of this which is convention rather
than mechanism.

---

## Inventory

### 1. `typesafe-api-key` — needed now

| | |
|---|---|
| secret id | `jev-tick-lab-typesafe-api-key` (created by Terraform, empty) |
| needed by | `-mode shadow`, and later `paper` / `live`. **Not** `observe` |
| read by | `deploy/run.sh`, at start-up, via the VM's own service-account token |
| granted to | the `jev-tick-lab` service account, `secretmanager.secretAccessor`, **on this secret only** |

Seed it:

```bash
printf %s "$TYPESAFE_API_KEY" | \
  gcloud secrets versions add jev-tick-lab-typesafe-api-key --data-file=-
```

`printf %s` rather than `echo`, so no trailing newline becomes part of the key.

Terraform creates the container and never a version. A secret version in
Terraform is a secret in the state file, which is a secret in a bucket that more
people can read than you think.

### 2. bitbank API credentials — phase 5 only, and not before

Phase 5 is live order placement. Phases 1–4 need no bitbank credentials at all:
the public stream is unauthenticated, and paper fills are simulated.

When that time comes, bitbank issues **two** values ([private REST auth](https://github.com/bitbankinc/bitbank-api-docs/blob/master/rest-api.md)):

- `ACCESS-KEY` — sent as a request header.
- the **API secret** — the HMAC-SHA256 signing key. Never transmitted, which
  makes it the more dangerous of the two.

**Store both in one secret as JSON**, not as two secrets. bitbank issues and
rotates them as a pair, and two separate secrets can drift: add a version to one
and forget the other, and every request fails signature validation with an error
that looks nothing like the cause.

```bash
# phase 5, not now
printf '{"key":"%s","secret":"%s"}' "$BITBANK_KEY" "$BITBANK_SECRET" | \
  gcloud secrets versions add jev-tick-lab-bitbank --data-file=-
```

Terraform does not create this container yet, deliberately — phase 4 and 5 are
not started and building their infrastructure early is the same mistake as
building the executor early. When it is time, it is these two blocks:

```hcl
resource "google_secret_manager_secret" "bitbank" {
  secret_id = "${var.name}-bitbank"
  replication { auto {} }
}

resource "google_secret_manager_secret_iam_member" "bot_reads_bitbank" {
  secret_id = google_secret_manager_secret.bitbank.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.bot.email}"
}
```

The binding is easy to forget, and its absence looks like a bug in `run.sh`
rather than a missing permission.

#### The permission that matters

bitbank grants API keys a choice of **参照 (read) / 取引 (trade) / 出金
(withdraw)** at issuance ([bitbank support](https://support.bitbank.cc/hc/ja/articles/360019410033)).

**Grant 参照 and 取引. Never 出金.**

This is the difference between two very different bad days. A trade-only key
that leaks lets an attacker place bad orders on your own account, bounded by
your balance and your own risk limits. A key with withdrawal permission lets
them take the funds. There is no phase of this experiment that needs to move
money off the exchange, so there is no argument for granting it.

Rule 7 in CLAUDE.md — reconcile position from the exchange on startup — needs
参照. Placing and cancelling orders needs 取引. That is the whole requirement.

#### If bitbank offers IP allowlisting

Not documented in the API repository; check the key issuance page. If it exists,
note the interaction with the current infrastructure: the VM uses an
**ephemeral** external IP, which changes every time it stops and starts. An
allowlist would need a reserved static address. That costs the same while
attached ($0.004/hr) and more while not ($0.01/hr), so reserve it only when
phase 5 actually needs it.

---

## Not secrets

Things that end up near this topic and do not belong in Secret Manager:

- **The GCS bucket and BigQuery dataset names.** Identifiers.
- **The alert email and billing account id.** In `terraform.tfvars`, which is
  gitignored — but as identifiers, not credentials.
- **The service account key.** There is none, and there should never be one. The
  VM authenticates through the metadata server; a downloadable JSON key is a
  credential on a disk somewhere, which is the thing this whole arrangement
  avoids.
- **The tick logs.** They contain market data, the model's answers, and the
  position. No credentials — see below.

---

## What leaves the machine

Worth being explicit, because "does my key end up somewhere" is the real
question behind all of this.

| goes out | contains |
|---|---|
| the `state` text sent to TypeSafe | market data and the position. No credentials |
| tick logs shipped to GCS | the same, plus the model's answers and any error string |
| the journal, to Cloud Logging | the same, at a coarser grain |

The error string is the interesting one: it is the only field that carries text
this code did not construct. `internal/jev` therefore redacts the credential
from every error it returns, including transport errors, and `Client` has a
`String` method so that printing one cannot print the key.

Neither should ever matter. Both are cheap, and the failure they prevent is a
key sitting in a BigQuery table forever.

---

## Rotation

```bash
printf %s "$NEW_KEY" | gcloud secrets versions add jev-tick-lab-typesafe-api-key --data-file=-
gcloud compute ssh jev-tick-lab --zone us-west1-b --tunnel-through-iap \
  -- sudo systemctl restart jev-tick-lab
```

`run.sh` fetches `versions/latest`, so adding a version plus a restart is the
whole procedure. Two things follow from the restart:

- It puts a gap in the tick series and writes a new row to
  `runs-YYYY-MM-DD.jsonl`. **Rotate between runs, not during one**, unless the
  key has actually been compromised.
- Disable the old version rather than destroying it, until the new one has
  produced a healthy `logcheck` verdict. A destroyed version cannot be rolled
  back to.

To revoke in a hurry: disable the version in Secret Manager and stop the unit.
The running process already holds the old key in memory, so the secret alone is
not a kill switch — stopping the service is.
