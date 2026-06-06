output "instance_connection_name" {
  description = "Cloud SQL instance connection name (used in proxy sidecar args and DSN)"
  value       = google_sql_database_instance.main.connection_name
}

output "db_user" {
  description = "Database username"
  value       = google_sql_user.app.name
}

output "db_password" {
  description = "Database password"
  value       = random_password.db.result
  sensitive   = true
}

output "proxy_gsa_email" {
  description = "GCP service account email for the Cloud SQL Auth Proxy (used in K8s SA annotation)"
  value       = google_service_account.proxy.email
}
