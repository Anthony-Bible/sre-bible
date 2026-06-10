resource "google_service_account" "proxy" {
  account_id   = "sre-bible-proxy"
  display_name = "sre-bible Cloud SQL Auth Proxy"
}

resource "google_project_iam_member" "cloudsql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.proxy.email}"
}

# The proxy GSA is the pod's de-facto application identity via Workload Identity
# (see the binding below), so app-level roles also land here. This grant lets the
# server call Model Armor's SanitizeUserPrompt via ADC. See ADR 0011.
resource "google_project_iam_member" "modelarmor_user" {
  project = var.project_id
  role    = "roles/modelarmor.user"
  member  = "serviceAccount:${google_service_account.proxy.email}"
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.proxy.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.k8s_namespace}/${var.k8s_service_account}]"
}
