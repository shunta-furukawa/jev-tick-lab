variable "project_id" {
  description = "GCP project that will hold the VM, the log bucket and the API key."
  type        = string
}

variable "region" {
  description = <<-EOT
    us-west1 (Oregon) for the initial experiment, decided by the owner on cost
    grounds — see the region note in CLAUDE.md.

    Two reasons it is us-west1 specifically and not us-central1:

      1. It is one of the three regions (with us-central1 and us-east1) where an
         e2-micro is covered by the Always Free tier. asia-northeast1 is not.
      2. api.typesafe.ai resolves into AWS us-west-2, which is also Oregon. So
         the model call — the thing Thresholds.MaxDecisionAge gates every signal
         on — goes from a ~100ms round trip out of Tokyo to roughly 10ms.

    What it costs: bitbank is in Tokyo, so the book arrives about 55ms later
    than it would in asia-northeast1. At a 3s cadence that is under 2% of a
    tick, which is fine for collecting a shadow dataset and is not fine for
    placing orders. Move back to asia-northeast1 before phase 4.
  EOT
  type        = string
  default     = "us-west1"
}

variable "zone" {
  type    = string
  default = "us-west1-b"
}

variable "name" {
  description = "Prefix for every resource this config creates."
  type        = string
  default     = "jev-tick-lab"
}

variable "machine_type" {
  description = <<-EOT
    e2-micro, and specifically e2-micro: it is the only machine type the Always
    Free tier covers, and only in us-central1, us-west1 and us-east1. One
    always-on instance fits inside the monthly allowance exactly.

    The caveat is its 0.25 vCPU baseline, which throttles under sustained load.
    This workload is bursty — one WebSocket, a few hundred book levels, one
    HTTPS call every few seconds — and if it does throttle it is not a silent
    failure: it shows up in logcheck as a record rate below the floor.

    Changing this forfeits the free tier. e2-small is about $12/month in
    us-west1.
  EOT
  type        = string
  default     = "e2-micro"
}

variable "disk_type" {
  description = <<-EOT
    pd-standard, because the free tier's 30 GB-months covers "standard
    persistent disk" only. pd-balanced would be about $0.10/GB/month, so around
    $3/month for the same 30GB.

    The tick logger appends a few KB every few seconds, which an HDD-backed disk
    does not notice. Boot and apt are slower; that is the trade.
  EOT
  type        = string
  default     = "pd-standard"
}

variable "disk_gb" {
  description = <<-EOT
    30GB is exactly the Always Free allowance for standard persistent disk.
    Going over forfeits it for the excess.

    At a 3s cadence a day of ticks is roughly 30MB — ~28,800 records carrying
    the state text they were evaluated against — so 30GB holds years. Nothing
    prunes the local copy; the shipper copies to GCS and never deletes.
  EOT
  type        = number
  default     = 30
}

variable "pair" {
  description = "bitbank pair. One pair per process — run a second VM for a second pair."
  type        = string
  default     = "xrp_jpy"
}

variable "model" {
  description = <<-EOT
    A pinned, versioned model id. Never an alias: `jev-latest` moves without
    notice and a moved model invalidates every tuned threshold (CLAUDE.md rule
    5). logcheck fails the run if the version changes mid-window.
  EOT
  type        = string
  default     = "jev-1.13.0"

  validation {
    condition     = can(regex("^jev-[0-9]+\\.[0-9]+\\.[0-9]+$", var.model))
    error_message = "Pin a versioned model id such as jev-1.13.0, not an alias."
  }
}

variable "tick" {
  description = <<-EOT
    Evaluation cadence. 3s for the initial experiment, decided by the owner on
    cost grounds: the model calls are ~90% of the bill and they scale linearly
    with this, so 1s -> 3s takes a five-day run from about $27 to about $9.

    It does change the dataset. The premise in CLAUDE.md is a once-per-second
    judgement, and a 3s series is a coarser one — fine for establishing whether
    confidence is calibrated at all, which is what phase 2 and 3 are for.

    Two things this has to stay consistent with: logcheck derives the expected
    record count from it, and Thresholds.MaxDecisionAge (2s) must still exceed
    the real call latency, which cmd/preflight measures.
  EOT
  type        = string
  default     = "3s"
}

variable "mode" {
  description = "observe or shadow. paper and live are later phases and the binary refuses them."
  type        = string
  default     = "shadow"

  validation {
    condition     = contains(["observe", "shadow"], var.mode)
    error_message = "Only observe and shadow exist. Phases 4 and 5 are not built."
  }
}

variable "log_bucket_name" {
  description = "Defaults to <project>-<name>-logs."
  type        = string
  default     = ""
}

variable "ssh_source_ranges" {
  description = <<-EOT
    Who may reach port 22. The default is Google's IAP forwarding range, so SSH
    goes through `gcloud compute ssh --tunnel-through-iap` and the VM answers
    nothing from the open internet.
  EOT
  type        = list(string)
  default     = ["35.235.240.0/20"]
}

variable "alert_email" {
  description = <<-EOT
    Where to send the "collection has stopped" mail. Leave empty to create no
    alerting at all.

    Phase 2 is a multi-day unattended run. Without this, the failure mode is
    discovering in phase 3 that Tuesday is missing.
  EOT
  type        = string
  default     = ""
}

variable "billing_account" {
  description = <<-EOT
    Billing account id (as in `gcloud billing accounts list`). Set it and the
    apply also creates a budget that mails you at 50/90/100% of budget_jpy.

    Worth doing on a personal project: nothing here can run away, but a forgotten
    VM costs about $12/month forever, and a forgotten collector costs ten times
    that in model calls.
  EOT
  type        = string
  default     = ""
}

variable "budget_usd" {
  description = "Monthly budget to alert against, in USD. Only used when billing_account is set."
  type        = number
  default     = 50
}
