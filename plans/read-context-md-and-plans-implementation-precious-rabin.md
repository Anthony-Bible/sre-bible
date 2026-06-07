# Phase 4: Polish + Deploy — sre.bible Resume Agent

## Context

Phases 1–3 are complete: ingestion pipeline, RAG query pipeline, and the HTTP/SSE chat server all work locally against docker-compose Postgres. Phase 4 takes the Resume Agent to production on the **existing GKE cluster** (`gke_gen-lang-client-0479899208_us-central1-f_prod`, k8s v1.35) at `https://sre.bible`, per the roadmap and ADR 0002 (Cloud SQL + pgvector, Auth Proxy). The repo currently has **zero deploy artifacts**, and `cmd/server` lacks health endpoints and graceful shutdown — prerequisites for sane rollouts.

**Cluster reconnaissance (verified via kubectl/gcloud):**
- ingress-nginx (class `nginx`, external IP `34.61.118.235`)
- cert-manager with ClusterIssuer `letsencrypt-prod` (READY) — TLS certs auto-issued via the `cert-manager.io/cluster-issuer` annotation (endixium convention). **No manual TLS secret needed.**
- external-dns with `--source=ingress --provider=cloudflare --cloudflare-proxied` — **DNS records auto-created in Cloudflare, proxied**, from Ingress hosts. No Terraform Cloudflare provider needed.
- ArgoCD active; newest app (endixium) is ArgoCD-managed with dedicated namespace
- Workload Identity enabled: pool `gen-lang-client-0479899208.svc.id.goog`

## Decisions (grilled & locked)

| Decision | Choice |
|---|---|
| Target cluster | Existing GKE `prod` cluster (project `gen-lang-client-0479899208`, `us-central1-f`) |
| Manifests | In-repo `deploy/` dir, raw YAML |
| Deploy tool | **ArgoCD Application** watching this repo's `deploy/` path (matches endixium convention); deploy = digest bump committed to `main`, ArgoCD syncs |
| Namespace | Dedicated `sre-bible` (newest-app convention) |
| Image / CI | `ghcr.io/anthony-bible/sre-bible`, GitHub Actions on push to `main`, manifest pinned **by digest** |
| Go toolchain | Bump to latest **Go 1.26.x** |
| Infra (Terraform) | `deploy/terraform/`, GCS state: Cloud SQL `db-f1-micro` PG17 (public IP, no authorized networks) + DB + user, proxy GCP SA + `roles/cloudsql.client` + WI binding. ~~Cloudflare DNS~~ handled by external-dns |
| TLS / DNS | cert-manager `letsencrypt-prod` annotation + `tls.secretName` on the Ingress; external-dns creates the proxied Cloudflare record from the Ingress host |
| Re-ingestion | Local `cloud-sql-proxy` + existing `ingest` CLI, `DATABASE_URL=localhost`. No in-cluster ingest |
| Secrets | Manual `kubectl create secret` (`sre-bible-secrets` in ns `sre-bible`), documented in runbook |
| Hardening | `/healthz` + `/readyz` (DB ping), graceful shutdown (SSE drain), JSON slog via `LOG_FORMAT=json`. Prometheus metrics **deferred → GitHub issue** |
| UI polish | Header one-liner bio + tuned suggested questions — **drafted from ingested Sources, user reviews before merge** |

No CONTEXT.md changes needed — Phase 4 introduces no new domain terms.

## Implementation stages

### Stage A — Code hardening (first: probes depend on it)
1. **`internal/server/server.go`** — add consumed-here port `Pinger interface { Ping(ctx context.Context) error }` (satisfied by `*pgxpool.Pool`; compile-time assertion beside the existing ones at `cmd/server/main.go:19`). Add field to `Server`, extend `NewServer`, register `GET /healthz`, `GET /readyz`.
2. **`internal/server/handlers.go`** — `handleHealthz`: 200 always, no DB (DB outage must not restart-loop the pod). `handleReadyz`: `Ping` with 2s timeout → 200 / 503.
3. **`cmd/server/main.go`** — `LOG_FORMAT=json` → `slog.NewJSONHandler` (text default); `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`; `ListenAndServe` in goroutine; on signal `httpSrv.Shutdown` with **30s drain** so in-flight SSE answers finish (`WriteTimeout` stays omitted; the `context.WithoutCancel` persist in `handlers.go:139` already survives cancellation).
4. **`internal/db/db.go`** — pin `cfg.MaxConns = 5` (+ `MaxConnIdleTime` 5m, `MaxConnLifetime` 30m): db-f1-micro allows ~25 conns; 2 replicas × 5 + local ingest leaves headroom. **Fix credential leak: `db.go:31` logs the full DSN with password — redact.**
5. **Tests (contract, not implementation)** — `internal/server/health_test.go`: `/healthz` 200 with failing Pinger stub; `/readyz` 200 healthy / 503 failing. Shutdown drain test (in-flight request completes, new ones refused).
6. **UI polish** — draft one-liner bio (header, `templates/index.html:219-222`) and 3–4 tuned suggested questions (`server.go:48`) from ingested source material → **present to user for approval** → apply.

### Stage B — Toolchain + containerization
7. **Go bump** — update `go.mod` to latest Go 1.26.x; run full test suite.
8. **`Dockerfile`** — multi-stage: `golang:1.26` builder, `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" ./cmd/server`; runtime `gcr.io/distroless/static-debian12:nonroot` (CA certs needed for Anthropic/Gemini HTTPS — rules out scratch). Templates + migrations are `go:embed`'d.
9. **`.dockerignore`** — `.git`, `bin/`, `*.test`, stray `query` binary, `plans/`, `docs/`.

### Stage C — CI
10. **`.github/workflows/build.yml`** — on push to `main` (+ `workflow_dispatch`): checkout → buildx → GHCR login (`GITHUB_TOKEN`) → `docker/metadata-action` (`type=sha`, `latest`) → `docker/build-push-action@v6`; echo digest to `$GITHUB_STEP_SUMMARY` for the digest-bump commit.
11. After first push: **make the GHCR package public** (else `ImagePullBackOff` — both kubelet and ArgoCD need pull access; new packages are private by default).

### Stage D — Terraform (`deploy/terraform/`) — GCP only
12. Files: `versions.tf` (google provider, **GCS backend**), `variables.tf` (`project_id=gen-lang-client-0479899208`, `region=us-central1`, `db_tier=db-f1-micro`, `db_version=POSTGRES_17`, `k8s_namespace=sre-bible`, `k8s_service_account=sre-bible`), `cloudsql.tf` (instance, database, user with `random_password`), `iam.tf` (proxy GSA, `roles/cloudsql.client`, WI binding `serviceAccount:gen-lang-client-0479899208.svc.id.goog[sre-bible/sre-bible]` → `roles/iam.workloadIdentityUser`), `outputs.tf` (`instance_connection_name`, `db_user`, `db_password` sensitive, `proxy_gsa_email`), `terraform.tfvars.example`.
13. `terraform init/plan/apply`; capture `instance_connection_name` for the sidecar args and the DSN.

### Stage E — Secrets + re-ingestion (needs live instance)
14. `kubectl create namespace sre-bible` (or let ArgoCD's `CreateNamespace=true` make it — secret needs it first, so create explicitly).
15. `kubectl create secret generic sre-bible-secrets -n sre-bible` — `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `DATABASE_URL=postgres://USER:PASS@127.0.0.1:5432/DB?sslmode=disable` (loopback to proxy sidecar; the proxy provides the mTLS tunnel to Cloud SQL).
16. Re-ingest: local `cloud-sql-proxy <INSTANCE_CONNECTION_NAME>` + `ingest` CLI per Source. **This also runs migrations once, single-runner** — deliberately sidesteps the multi-replica goose race (goose `UpContext` takes no advisory lock). Optional `make ingest-prod` target.

### Stage F — Manifests (`deploy/`, one resource per file) + ArgoCD
17. `deploy/serviceaccount.yaml` — SA `sre-bible`, annotation `iam.gke.io/gcp-service-account: <proxy-gsa>@gen-lang-client-0479899208.iam.gserviceaccount.com`.
18. `deploy/deployment.yaml` — `replicas: 2`, image **by digest**, `containerPort: 8080` named `http`; env from `sre-bible-secrets` + `LOG_FORMAT=json`; probes: liveness `/healthz`, readiness `/readyz`; resources 128Mi/10m → 512Mi/500m (password-exchange convention); `terminationGracePeriodSeconds: 40` (> 30s drain); **Cloud SQL Auth Proxy as native sidecar** — `initContainers` entry with `restartPolicy: Always` (k8s 1.35 ✓), image `gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.x` (pinned), args `["--structured-logs", "--port=5432", "<INSTANCE_CONNECTION_NAME>"]` — no key file (WI), no `--private-ip`.
19. `deploy/service.yaml` — port 80 → `http`.
20. `deploy/ingress.yaml` — `ingressClassName: nginx`, host `sre.bible`; annotations: `cert-manager.io/cluster-issuer: letsencrypt-prod` (endixium convention), `nginx.ingress.kubernetes.io/proxy-buffering: "off"`, `proxy-read-timeout: "300"`, `proxy-send-timeout: "300"` (SSE; app's `X-Accel-Buffering: no` at `handlers.go:93` is the per-response belt to this suspenders); `tls:` block with `secretName: sre-bible-tls` (cert-manager issues it). **external-dns auto-creates the proxied Cloudflare record from this host.**
21. `deploy/argocd-application.yaml` — ArgoCD `Application` (ns `argocd`): source `https://github.com/Anthony-Bible/sre-bible`, path `deploy`, targetRevision `main`, destination ns `sre-bible`; `syncPolicy.automated` (prune + selfHeal) + `syncOptions: [CreateNamespace=true]`. Exclude itself from the watched path (place at `deploy/argocd/application.yaml` and set Application path to `deploy` with `directory.exclude: argocd/**`, or simply keep it at repo root `deploy/argocd-application.yaml` outside the synced dir — pick during implementation to match how endixium's Application is registered). Applied **once** manually: `kubectl apply -f`.
22. Push to `main` with the CI digest pinned → ArgoCD syncs. Verify Cloudflare zone SSL mode is **Full (strict)** (origin presents a real Let's Encrypt cert).

### Stage G — Docs + deferred
23. **`deploy/README.md`** runbook: Terraform apply order, secret creation, digest-bump deploy flow (commit → ArgoCD sync), re-ingest procedure, first-deploy migration note.
24. **`docs/adr/0004-deployment-topology.md`** — GKE + ingress-nginx + cert-manager (LE) + external-dns (Cloudflare proxied) + ArgoCD GitOps + Terraform for Cloud SQL/IAM only; records why no Origin CA cert and no Terraform DNS.
25. `gh issue create` — "Add Prometheus metrics endpoint" (promhttp, scrape annotations). Deferred per grilling.

## Verification

- **Unit/contract:** `make test-unit` + new health/shutdown tests; full suite after Go 1.26 bump.
- **Container:** `docker build` + run against compose Postgres (`make db-up`); curl `/healthz`, `/readyz`; confirm JSON logs.
- **CI:** push → image + digest in GHCR job summary.
- **Terraform:** `plan` review → `apply` → `gcloud sql instances describe`; WI member matches `sre-bible/sre-bible`.
- **In-cluster:** ArgoCD app Synced/Healthy; pods Ready (app + proxy sidecar); `kubectl port-forward -n sre-bible svc/sre-bible 8080:80` → `/readyz` 200 (proves app→proxy→Cloud SQL).
- **Edge:** `kubectl get certificate -n sre-bible` Ready; `dig sre.bible` resolves to Cloudflare; `curl -N https://sre.bible/chat -d 'question=...'` — incremental `event: token` frames (not one buffered blob), then `done`.
- **End-to-end:** browser at `https://sre.bible` — valid TLS, suggested question streams an answer with citations; `kubectl rollout restart` mid-stream completes the in-flight answer.

## Risks / gotchas (pre-mitigated)

1. **goose migration race** with `replicas: 2` → migrations applied first via single local ingest run (Stage E); follow-up option: goose provider API + `WithSessionLocker()`.
2. **db-f1-micro ~25-conn ceiling** → `MaxConns=5`.
3. **GHCR private by default** → make package public.
4. **external-dns zone coverage** — verify the `sre.bible` zone exists in the Cloudflare account external-dns's token can edit (no `--domain-filter` is set, so any owned zone works). If the zone is in a different account, fall back to a manual DNS record.
5. **DSN logged with password** (`db.go:31`) → redacted in Stage A.
6. **ArgoCD repo access** — public GitHub repo, no credentials needed; confirm how endixium's Application/repo is registered and mirror it.
7. Cloudflare SSE read-timeouts: non-issue at haiku answer latencies; noted in runbook.
