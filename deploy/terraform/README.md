# sre-bible Terraform Module

Provisions the GCP infrastructure for sre-bible: Cloud SQL (PostgreSQL + pgvector), a Cloud SQL Auth Proxy service account, and Workload Identity binding for GKE.

## Resources

| File | Resources |
|------|-----------|
| `cloudsql.tf` | `random_password.db`, `google_sql_database_instance.main`, `google_sql_database.main`, `google_sql_user.app` |
| `iam.tf` | `google_service_account.proxy`, `google_project_iam_member.cloudsql_client`, `google_service_account_iam_member.workload_identity` |

## Usage

```hcl
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars — set project_id at minimum
terraform init
terraform plan
terraform apply
```

## Inputs

| Name | Description | Type | Default | Required |
|------|-------------|------|---------|----------|
| `project_id` | GCP project ID | `string` | — | yes |
| `region` | GCP region for all resources | `string` | `"us-central1"` | no |
| `db_tier` | Cloud SQL machine tier | `string` | `"db-f1-micro"` | no |
| `db_version` | Cloud SQL PostgreSQL engine version | `string` | `"POSTGRES_18"` | no |
| `k8s_namespace` | Kubernetes namespace for the workload | `string` | `"sre-bible"` | no |
| `k8s_service_account` | Kubernetes service account name | `string` | `"sre-bible"` | no |

## Outputs

| Name | Description | Sensitive |
|------|-------------|-----------|
| `instance_connection_name` | Cloud SQL connection name for proxy sidecar | no |
| `db_user` | Database username | no |
| `db_password` | Database password | yes |
| `proxy_gsa_email` | GCP service account email for Workload Identity annotation | no |