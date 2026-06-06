resource "google_service_account" "proxy" {
  account_id   = "sre-bible-proxy"
  display_name = "sre-bible Cloud SQL Auth Proxy"
}

resource "google_project_iam_member" "cloudsql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.proxy.email}"
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.proxy.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.k8s_namespace}/${var.k8s_service_account}]"
}
