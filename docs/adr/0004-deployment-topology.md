# ADR 0004 — Deployment Topology

## Status
Accepted

## Context
Phase 4 deploys the sre.bible Resume Agent to production. The existing GKE cluster (`gen-lang-client-0479899208`, `us-central1-f`) already runs ingress-nginx, cert-manager (letsencrypt-prod), external-dns (Cloudflare proxied), ArgoCD, and has Workload Identity enabled. We need to decide: where to host, how to handle TLS/DNS, database, secrets, CI/CD, and deploy workflow.

## Decision
Deploy the application to the existing GKE cluster using raw Kubernetes YAML manifests managed by ArgoCD, with Cloud SQL (postgres + pgvector) provisioned by Terraform and accessed via a Cloud SQL Auth Proxy native sidecar. GitHub Actions builds and publishes the image to GHCR; deploys are triggered by bumping the image digest in `deploy/deployment.yaml` on main.

## Rationale
1. **GKE (existing cluster) over Cloud Run** — the existing cluster already runs all needed controllers (ingress-nginx, cert-manager, external-dns, ArgoCD); no new platform to manage. Cloud Run's SSE support requires newer features and introduces billing complexity not justified for a single-operator project.
2. **Raw YAML in `deploy/` over Helm** — Helm adds indirection for a small single-app project. Raw YAML is readable, diffable, and ArgoCD handles it natively without a chart abstraction layer.
3. **ArgoCD GitOps (digest-bump to main → auto-sync) over manual `kubectl apply`** — matches how the existing endixium app is deployed; git provides an audit trail; ArgoCD's self-heal prevents cluster drift.
4. **Terraform only for Cloud SQL + IAM; not for DNS** — external-dns auto-creates Cloudflare proxied records from `Ingress` hosts, so no Terraform Cloudflare provider is needed. cert-manager issues Let's Encrypt certs automatically via the `cert-manager.io/cluster-issuer` annotation — no manual TLS secrets or Terraform-managed certs.
5. **Cloud SQL Auth Proxy as native sidecar (`initContainer` + `restartPolicy: Always`)** — Kubernetes 1.35 supports the native sidecar pattern; the proxy runs with Workload Identity (no key file); the app container connects over loopback (`127.0.0.1:5432`), avoiding network exposure.
6. **No Origin CA cert; Full (strict) SSL mode** — cert-manager issues a real Let's Encrypt cert, so Cloudflare Full (strict) mode works without an Origin CA cert. Using an Origin CA cert would require either Full (non-strict) mode or rotating the cert manually.
7. **Migrations via single local ingest run** — `goose`'s `UpContext` takes no advisory lock; running migrations from a single local process (via `cloud-sql-proxy` port-forward) before deploying sidesteps the multi-replica race condition. Follow-up option: goose provider API + `WithSessionLocker()`.
8. **MaxConns=5 per replica** — `db-f1-micro` allows ~25 connections; 2 replicas × 5 + local ingest headroom fits within that ceiling safely.

## Consequences
- Standard GitOps workflow: merge to main → image build → digest bump → ArgoCD auto-sync.
- TLS and DNS are fully automated; no manual cert or DNS record management.
- Database is managed by Terraform with state in GCS; schema migrations are a manual operator step before each deploy that changes the schema.
- Secrets are managed manually via `kubectl create secret` — acceptable for a single-operator project; a future improvement would be External Secrets Operator backed by GCP Secret Manager.
