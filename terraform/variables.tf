variable "project_id" {
  description = "GCP project that will hold the VM, the log bucket and the API key."
  type        = string
}

variable "region" {
  description = "bitbank is a domestic Japanese exchange; keep the network hop short."
  type        = string
  default     = "asia-northeast1"
}

variable "zone" {
  type    = string
  default = "asia-northeast1-b"
}

variable "name" {
  description = "Prefix for every resource this config creates."
  type        = string
  default     = "jev-tick-lab"
}

variable "machine_type" {
  description = <<-EOT
    e2-micro is the default because the workload really is tiny: one WebSocket,
    a few hundred book levels, and one HTTPS call a second. In asia-northeast1
    it is $7.84/month against $15.69 for e2-small (Sep 2026 list; E2 gets no
    sustained-use discount, so the sticker price is the price).

    The caveat is e2-micro's 0.25 vCPU baseline, which throttles under sustained
    load. This workload is bursty, not sustained, so it should be fine — and if
    it is not, you will know: throttling shows up in logcheck as a record rate
    below the floor. Move to e2-small then, not before.
  EOT
  type        = string
  default     = "e2-micro"
}

variable "disk_type" {
  description = <<-EOT
    pd-balanced at $0.13/GB/month, against $0.052 for pd-standard — $1.56 a
    month on a 20GB disk. Not worth the sluggish boot and apt runs of an
    HDD-backed disk, but pd-standard is there if every yen counts. The tick
    logger writes about 3KB a second, which neither type notices.
  EOT
  type        = string
  default     = "pd-balanced"
}

variable "disk_gb" {
  description = <<-EOT
    A day of ticks is roughly 90MB: ~29,000 records carrying the state text they
    were evaluated against. 20GB holds most of a year. Nothing prunes the local
    copy — the shipper copies to GCS and never deletes — so this is the real
    bound on an unattended run.
  EOT
  type        = number
  default     = 20
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
