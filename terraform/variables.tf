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
    e2-small rather than e2-micro on purpose. The workload is tiny, but the tick
    loop is latency-sensitive and e2-micro's 0.25 vCPU baseline can throttle
    exactly when a burst of depth diffs arrives. e2-micro is about half the
    price if the run turns out not to need the headroom — check the skipped-tick
    count in logcheck before downgrading.
  EOT
  type        = string
  default     = "e2-small"
}

variable "disk_gb" {
  description = <<-EOT
    A day of ticks is roughly 90MB: ~29,000 records carrying the state text they
    were evaluated against. 20GB holds most of a year, and the log shipper is
    what actually bounds it.
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
