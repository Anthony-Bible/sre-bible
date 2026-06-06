variable "project_id" {
  description = "GCP project ID"
  type        = string
  default     = "gen-lang-client-0479899208"
}

variable "region" {
  description = "GCP region for Cloud SQL"
  type        = string
  default     = "us-central1"
}

variable "db_tier" {
  description = "Cloud SQL machine tier"
  type        = string
  default     = "db-f1-micro"
}

variable "db_version" {
  description = "Postgres version"
  type        = string
  default     = "POSTGRES_17"
}

variable "k8s_namespace" {
  description = "Kubernetes namespace for the workload"
  type        = string
  default     = "sre-bible"
}

variable "k8s_service_account" {
  description = "Kubernetes service account name"
  type        = string
  default     = "sre-bible"
}
