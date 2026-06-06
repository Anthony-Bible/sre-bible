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

    database_flags {
      name  = "cloudsql.enable_pgvector"
      value = "on"
    }

    ip_configuration {
      ipv4_enabled = true
      # No authorized_networks — Cloud SQL Auth Proxy handles auth via IAM
    }

    backup_configuration {
      enabled = true
    }
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
