# sre.bible — Operations Runbook

This runbook covers first deployment and ongoing operations. The GKE cluster is
`gke_gen-lang-client-0479899208_us-central1-f_prod`. ArgoCD manages everything
under `deploy/` after initial bootstrap.

---

## Prerequisites

```bash
gcloud auth login
gcloud config set project gen-lang-client-0479899208
gcloud container clusters get-credentials prod --zone us-central1-f
kubectl config current-context   # must print gke_gen-lang-client-0479899208_us-central1-f_prod
```

---

## First Deploy

### 1. Provision Cloud SQL with Terraform

```bash
cd deploy/terraform
terraform init
terraform plan -out=tfplan
terraform apply tfplan
```

Note the outputs — you will need them in later steps:

| Output | Use |
|---|---|
| `instance_connection_name` | Cloud SQL Auth Proxy arg in `deployment.yaml` |
| `db_user` | DATABASE_URL |
| `db_password` (sensitive) | DATABASE_URL |
| `proxy_gsa_email` | Workload Identity binding (already wired by Terraform) |

Retrieve the sensitive password:

```bash
terraform output -raw db_password
```

### 2. Create Namespace

```bash
kubectl create namespace sre-bible
```

### 3. AWS SES Setup (one-time per AWS account)

SES is not managed by Terraform (GCP-only).

#### 3a. Verify sender identity

`server@password.exchange` must be verified in SES (us-west-2). If not already verified:

```bash
aws ses verify-email-identity --email-address server@password.exchange --region us-west-2
```

Click the confirmation link in the inbox.

#### 3b. Confirm account is out of sandbox

In the SES console (us-west-2), check **Account dashboard**. If still in sandbox, request production access. The account is currently out of sandbox.

#### 3c. Create a dedicated IAM user

```bash
aws iam create-user --user-name sre-bible-ses
aws iam put-user-policy --user-name sre-bible-ses \
  --policy-name SendEmailOnly \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Action": "ses:SendEmail",
      "Resource": "*"
    }]
  }'
aws iam create-access-key --user-name sre-bible-ses
```

Save the printed `AccessKeyId` and `SecretAccessKey` — you will need them in the next step.

### 4. Create Secrets

```bash
kubectl create secret generic sre-bible-secrets -n sre-bible \
  --from-literal=ANTHROPIC_API_KEY=<key> \
  --from-literal=GEMINI_API_KEY=<key> \
  --from-literal=DATABASE_URL="postgres://sre_bible:<password>@127.0.0.1:5432/sre_bible?sslmode=disable" \
  --from-literal=AWS_ACCESS_KEY_ID=<aws-access-key-id> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<aws-secret-access-key> \
  --from-literal=EMAIL_TO=<destination-email-address> \
  --from-literal=EMAIL_FROM=<ses-verified-sender-address> \
  --from-literal=TURNSTILE_SECRET_KEY=<cloudflare-turnstile-secret-key>
```

> The AWS keys belong to a dedicated IAM user with a policy scoped to
> `ses:SendEmail` only. See [ADR 0006](../docs/adr/0006-aws-ses-for-contact-email.md)
> and the AWS SES setup in step 3 above.

> `TURNSTILE_SECRET_KEY` is the server-side secret from the Cloudflare Turnstile
> dashboard (distinct from the public site key in `deployment.yaml`). The secret
> **must be applied before ArgoCD syncs the new deployment** — the server fails
> fast at startup when this key is absent. For local dev use the Cloudflare
> always-pass test secret: `1x0000000000000000000000000000000AA`.

> `DATABASE_URL` uses `127.0.0.1` — the loopback address to the Cloud SQL Auth
> Proxy sidecar running in the same Pod. `sslmode=disable` is correct here: the
> proxy handles mTLS to Cloud SQL; the intra-Pod connection is already on
> loopback.

### 4b. Grant Model Armor access (one-time)

The inbound prompt gate calls Google Cloud Model Armor via Workload Identity
(ADC) — there is no API key. The `MODEL_ARMOR_TEMPLATE` resource name is set as a
plain (non-secret) env var in `deployment.yaml`; the server **fails fast at
startup when it is unset**, so it must be present before ArgoCD syncs. The bound
GSA needs `roles/modelarmor.user`, and the `sre-bible` template must exist in
`us-central1` with Prompt-Injection & Jailbreak detection enabled. See
[ADR 0011](../docs/adr/0011-model-armor-prompt-gate.md).

```bash
gcloud services enable modelarmor.googleapis.com --project gen-lang-client-0479899208
gcloud projects add-iam-policy-binding gen-lang-client-0479899208 \
  --member="serviceAccount:sre-bible-proxy@gen-lang-client-0479899208.iam.gserviceaccount.com" \
  --role="roles/modelarmor.user"
```

### 5. Update deployment.yaml

Replace the placeholder in `deploy/deployment.yaml` with the real
`instance_connection_name` from Terraform output:

```yaml
# in the cloud-sql-proxy initContainer args
- "--instances=<INSTANCE_CONNECTION_NAME>=tcp:5432"
```

### 6. Run Migrations and Ingest

Migrations must run from a **single process** before deploying — goose does not
hold an advisory lock, so running from 2 replicas simultaneously causes races.

```bash
# Start a local proxy on port 5433 (avoids conflict with any local postgres)
cloud-sql-proxy <INSTANCE_CONNECTION_NAME> --port=5433 &

# Run schema migrations
DATABASE_URL="postgres://sre_bible:<password>@127.0.0.1:5433/sre_bible?sslmode=disable" \
  go run ./cmd/ingest migrate

# Ingest sources (repeat for each source URL or local path)
DATABASE_URL="postgres://sre_bible:<password>@127.0.0.1:5433/sre_bible?sslmode=disable" \
  GEMINI_API_KEY="<key>" \
  go run ./cmd/ingest <source-url-or-path>

# Stop the proxy
kill %1
```

Or equivalently: `make ingest-prod` (see Makefile for the full incantation).

### 7. Bootstrap ArgoCD Application

This is a one-time step. After this, ArgoCD continuously reconciles
`deploy/` from the `deploy-state` branch — the CD-managed release branch that
CI force-pushes on every deploy (see "Ongoing Deploys" below), **not** `main`.

`deploy-state` must exist before ArgoCD can sync. Seed it once from the current
`main` tip (or let the first push-to-`main` CI run create it before you apply
the Application):

```bash
git push origin main:deploy-state
```

```bash
kubectl apply -f deploy/argocd/application.yaml
```

Verify sync:

```bash
kubectl get application -n argocd sre-bible
```

### 8. Make GHCR Package Public

After the first GitHub Actions image push, make the package public so the
kubelet can pull without credentials:

1. GitHub → Settings → Packages → `sre-bible` → Package settings
2. Change visibility → **Public**

If you skip this step you will see `ImagePullBackOff` on all Pods.

### 9. Confirm Cloudflare SSL Mode

Ensure the Cloudflare zone SSL/TLS mode is set to **Full (strict)**. cert-manager
issues a real Let's Encrypt certificate so strict mode works without an Origin
CA cert.

---

## Ongoing Deploys (Automated Release-Branch Flow)

Deploys are fully automated — there is **no manual digest edit**. On every push
to `main`, the `render_and_deploy` job:

1. Builds the image and pushes it to GHCR.
2. Renders the resolved image digest into `deploy/deployment.yaml`.
3. Force-pushes the result ("latest `main` + digest") to the `deploy-state`
   branch.

ArgoCD tracks `deploy-state` (not `main`) and auto-syncs within ~3 minutes.

Why a separate branch: `main` is protected with required status checks
(`test-unit`/`build`/`eval`) that CI's default `GITHUB_TOKEN` cannot satisfy, so
pushing a digest bump there is rejected (`GH006: protected branch hook
declined`). The unprotected `deploy-state` branch sidesteps that while keeping
`main` protected and free of automated commits. Its history is disposable
(force-pushed each deploy).

Monitor the rollout:

```bash
kubectl rollout status -n sre-bible deployment/sre-bible
```

---

## Re-Ingest Procedure

Same as First Deploy step 5, but skip the `migrate` step if the schema has not
changed.

```bash
cloud-sql-proxy <INSTANCE_CONNECTION_NAME> --port=5433 &

DATABASE_URL="postgres://sre_bible:<password>@127.0.0.1:5433/sre_bible?sslmode=disable" \
  GEMINI_API_KEY="<key>" \
  go run ./cmd/ingest <source-url-or-path>

kill %1
```

---

## Troubleshooting

| Symptom | Command |
|---|---|
| TLS cert not issuing | `kubectl get certificate -n sre-bible` |
| Cloud SQL proxy errors | `kubectl logs -n sre-bible -l app=sre-bible -c cloud-sql-proxy` |
| Verify DB connectivity | `kubectl port-forward -n sre-bible svc/sre-bible 8080:80` then `curl localhost:8080/readyz` |
| Force rolling restart | `kubectl rollout restart -n sre-bible deployment/sre-bible` |
| ImagePullBackOff | Make GHCR package public (see First Deploy step 7) |
| ArgoCD not syncing | `kubectl get application -n argocd sre-bible -o yaml` — check `status.conditions` |
