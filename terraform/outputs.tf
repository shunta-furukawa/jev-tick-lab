output "instance" {
  value = google_compute_instance.bot.name
}

output "zone" {
  value = google_compute_instance.bot.zone
}

output "log_bucket" {
  value = google_storage_bucket.logs.name
}

output "bigquery_dataset" {
  value = google_bigquery_dataset.ticks.dataset_id
}

output "secret_id" {
  description = "Seed this before the first deploy; Terraform creates the container, not the value."
  value       = google_secret_manager_secret.typesafe_api_key.secret_id
}

output "service_account" {
  value = google_service_account.bot.email
}

output "ssh" {
  description = "There is no public SSH; this tunnels through IAP."
  value       = "gcloud compute ssh ${google_compute_instance.bot.name} --zone ${google_compute_instance.bot.zone} --tunnel-through-iap"
}

output "next_steps" {
  value = <<-EOT
    1. Seed the API key (Terraform never sees the value):
         printf %s "$TYPESAFE_API_KEY" | gcloud secrets versions add ${google_secret_manager_secret.typesafe_api_key.secret_id} --data-file=-
    2. Deploy the binary and the units:
         ./deploy/deploy.sh ${var.project_id} ${google_compute_instance.bot.name} ${google_compute_instance.bot.zone}
    3. Watch the first minutes:
         gcloud compute ssh ${google_compute_instance.bot.name} --zone ${google_compute_instance.bot.zone} --tunnel-through-iap -- journalctl -u ${var.name} -f
  EOT
}
