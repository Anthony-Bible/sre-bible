# ADR 0006 — AWS SES for Contact Email Delivery

## Status

Accepted

## Context

The Resume Agent needs to deliver Viewer-composed contact messages to the Owner (see CONTEXT.md: Contact Email). The system runs on GKE (GCP). GCP has no first-party transactional email equivalent comparable to AWS SES — Cloud Functions + Sendgrid is a common workaround but introduces additional vendor and complexity overhead. The Owner already operates AWS SES for other applications on the same GKE cluster, with a verified sender identity (`server@password.exchange`) and an account that is out of the SES sandbox. The existing usage represents an established cross-cloud pattern for this cluster.

Two SES integration styles were considered:

- **SMTP endpoint** — used by sibling cluster applications.
- **sesv2 API client** — typed SDK errors, IAM policy scope limited to `ses:SendEmail`, no SMTP credential derivation.

## Decision

Use the AWS SES v2 API (`sesv2.SendEmail`) via `aws-sdk-go-v2`. A dedicated IAM user is created for sre-bible, with an inline policy scoped strictly to `ses:SendEmail`. Static credentials are injected via Kubernetes secret (`sre-bible-secrets`) and consumed by the application with `credentials.NewStaticCredentialsProvider` — the default credential chain (IMDS, Workload Identity) is explicitly bypassed to prevent accidental credential probing on GKE nodes.

Key configuration:
- `EMAIL_FROM=server@password.exchange` (already-verified SES identity)
- `EMAIL_TO=sre-bible@anthonybible.com` (catch-all; marks origin for filtering)
- `AWS_REGION=us-west-2`
- `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` via `sre-bible-secrets`

SES setup (IAM user, verified identity, sandbox exit) is manual and outside the GCP Terraform stack.

## Consequences

- **Single cross-cloud dependency**: the Go binary takes a dependency on `aws-sdk-go-v2/service/sesv2`, bringing an AWS dependency tree into an otherwise GCP-only service.
- **Secrets**: two AWS keys live in `sre-bible-secrets` alongside the existing GCP credentials.
- **SMTP divergence**: sibling apps use the SES SMTP endpoint; sre-bible uses the API client. Both are supported by the same SES account — the divergence is intentional (scoped IAM vs. shared SMTP credential).
- **No Terraform coverage**: SES IAM and identity setup must be repeated manually if the AWS account changes.
- **Feature is opt-in at runtime**: if any required env var (`EMAIL_FROM`, `EMAIL_TO`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) is absent, the server starts without the contact email tool and logs a single info line. This allows progressive rollout without a code change.
