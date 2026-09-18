terraform {
  required_version = ">= 1.5"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }

  # State for a single experiment VM is still state worth keeping: losing it
  # means the next `terraform apply` tries to build a second of everything.
  # Create the bucket first (it cannot be managed by the config it stores):
  #
  #   gcloud storage buckets create gs://YOUR_PROJECT-tfstate \
  #     --location=asia-northeast1 --uniform-bucket-level-access
  #   gcloud storage buckets update gs://YOUR_PROJECT-tfstate --versioning
  #
  # then uncomment and run `terraform init -reconfigure`.
  #
  # backend "gcs" {
  #   bucket = "YOUR_PROJECT-tfstate"
  #   prefix = "jev-tick-lab"
  # }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
