resource "random_password" "db" {
  length  = 32
  special = false
}
resource "google_sql_database_instance" "main" {
  name             = "sre-bible"
  database_version = var.db_version
  region           = var.region

  settings {
    tier              = var.db_tier
    availability_type = "ZONAL"
    disk_autoresize   = true

    ip_configuration {
      ipv4_enabled = true
      # No authorized_networks — Cloud SQL Auth Proxy handles auth via IAM
    }

    backup_configuration {
      enabled = true
      # PITR: the instance is shared with password-exchange, whose previous
      # MySQL home had binlog PITR (ADR 0015). Turning this on restarts the
      # instance once.
      point_in_time_recovery_enabled = true
      transaction_log_retention_days = 7
    }

    # GCP-side guard (API/console deletes), on top of Terraform's
    # deletion_protection below. Two apps live on this instance.
    deletion_protection_enabled = true
  }

  deletion_protection = true
}

resource "google_sql_database" "main" {
  instance = google_sql_database_instance.main.name
  name     = "sre_bible"
}

resource "google_sql_user" "app" {
  instance = google_sql_database_instance.main.name
  name     = "sre_bible"
  password = random_password.db.result
}
