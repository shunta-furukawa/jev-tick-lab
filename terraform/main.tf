locals {
  log_bucket = var.log_bucket_name != "" ? var.log_bucket_name : "${var.project_id}-${var.name}-logs"
}

# --- network ----------------------------------------------------------------
#
# A dedicated VPC rather than the default network: the default one ships with
# an allow-SSH-from-anywhere rule, and this VM holds an API key.

resource "google_compute_network" "this" {
  name                    = var.name
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "this" {
  name          = var.name
  network       = google_compute_network.this.id
  region        = var.region
  ip_cidr_range = "10.20.0.0/24"

  # So that SSH through IAP does not need a public address to talk to.
  private_ip_google_access = true
}

# The only way in. Egress is left at the GCP default (allow all) because the
# bot needs to reach stream.bitbank.cc, api.typesafe.ai and googleapis.com.
resource "google_compute_firewall" "ssh_from_iap" {
  name          = "${var.name}-ssh-from-iap"
  network       = google_compute_network.this.name
  source_ranges = var.ssh_source_ranges
  target_tags   = [var.name]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# --- identity ---------------------------------------------------------------

resource "google_service_account" "bot" {
  account_id   = var.name
  display_name = "jev-tick-lab collector"
  description  = "Reads the TypeSafe key, writes tick logs to GCS, ships its journal to Cloud Logging."
}

# Deliberately NOT granted BigQuery. The VM's job is to collect and ship; the
# analysis load runs from a workstation, so a compromised VM cannot rewrite the
# dataset it has already delivered.
resource "google_project_iam_member" "logging" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.bot.email}"
}

resource "google_project_iam_member" "monitoring" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.bot.email}"
}

# --- the API key ------------------------------------------------------------
#
# The secret CONTAINER is managed here; the value is not. A secret version in
# Terraform is a secret in the state file, which is a secret in a bucket that
# more people can read than you think. Seed it by hand:
#
#   printf %s "$TYPESAFE_API_KEY" | gcloud secrets versions add jev-tick-lab-typesafe-api-key --data-file=-

resource "google_secret_manager_secret" "typesafe_api_key" {
  secret_id = "${var.name}-typesafe-api-key"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_iam_member" "bot_reads_key" {
  secret_id = google_secret_manager_secret.typesafe_api_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.bot.email}"
}

# --- where the data lands ---------------------------------------------------

resource "google_storage_bucket" "logs" {
  name                        = local.log_bucket
  location                    = var.region
  uniform_bucket_level_access = true
  storage_class               = "STANDARD"

  # The tick logs ARE the deliverable of this experiment, so there is no
  # deletion rule here and force_destroy stays false: `terraform destroy` must
  # not be able to throw away a week of collection.
  force_destroy = false

  lifecycle_rule {
    condition {
      age = 30
    }
    action {
      type          = "SetStorageClass"
      storage_class = "NEARLINE"
    }
  }
}

resource "google_storage_bucket_iam_member" "bot_writes_logs" {
  bucket = google_storage_bucket.logs.name
  role   = "roles/storage.objectAdmin" # rsync needs to list and overwrite, not just create
  member = "serviceAccount:${google_service_account.bot.email}"
}

resource "google_bigquery_dataset" "ticks" {
  dataset_id  = replace(var.name, "-", "_")
  location    = var.region
  description = "Tick records loaded from gs://${local.log_bucket}. Schema is autodetected from the JSONL; internal/obs.Record is the source of truth."

  # Same reasoning as the bucket.
  delete_contents_on_destroy = false
}

# --- the machine ------------------------------------------------------------

resource "google_compute_instance" "bot" {
  name         = var.name
  machine_type = var.machine_type
  zone         = var.zone
  tags         = [var.name]

  # So a machine_type change does not need a manual stop first.
  allow_stopping_for_update = true

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
      size  = var.disk_gb
      type  = var.disk_type
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.this.id

    # An ephemeral public address, purely for egress. The alternative is Cloud
    # NAT, which costs several times more than the VM it would be protecting,
    # and nothing listens on this host anyway.
    access_config {}
  }

  service_account {
    email = google_service_account.bot.email
    # Scopes are the old mechanism; the IAM roles above are what actually
    # decide, and cloud-platform lets them apply unmodified.
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    enable-oslogin = "TRUE"

    startup-script = templatefile("${path.module}/startup.sh", {
      name       = var.name
      project_id = var.project_id
      pair       = var.pair
      model      = var.model
      mode       = var.mode
      secret_id  = google_secret_manager_secret.typesafe_api_key.secret_id
      log_bucket = google_storage_bucket.logs.name
    })
  }

  # The startup script only prepares the machine. The binary and the units are
  # pushed by deploy/deploy.sh, so that shipping new code never means rebuilding
  # infrastructure — and so a redeploy cannot quietly lose the collected data.
  lifecycle {
    ignore_changes = [metadata["ssh-keys"]]
  }
}
