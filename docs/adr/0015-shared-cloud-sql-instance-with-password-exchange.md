# ADR 0015 — Share the Cloud SQL instance with password-exchange

## Status

Accepted. Supersedes decision 8 of ADR 0004 (MaxConns=5 per replica).

## Context

password-exchange is moving its relational data off a shared Cloud SQL MySQL
instance (password-exchange epic #562). Rather than paying for a second Postgres
instance, it gets its own databases, users and service account on this
project's `sre-bible` instance (POSTGRES_18, `db-f1-micro`, ZONAL).

`db-f1-micro` has 25 `max_connections`. The last 3 are reserved for
superusers, and `cloudsqladmin` uses ordinary slots, so about 19 are usable by
applications. Both apps now share that pool.

The MySQL instance password-exchange leaves has point-in-time recovery; this
instance had only daily backups.

## Decision

1. **One instance, two apps.** `sre-bible`'s Terraform (`deploy/terraform/`)
   keeps owning the instance and its settings. password-exchange's Terraform
   reads the instance through a data source and manages only its own databases,
   users and IAM.
2. **sre-bible's pool drops to `MaxConns = 4` per process** (2 replicas → 8).
   password-exchange caps its pools to fit the rest; its budget lives in
   password-exchange issue #568.
3. **PITR on (7 days of transaction logs) and GCP deletion protection on.**
4. **No tier bump and no Managed Connection Pooling** (Enterprise Plus only, and
   transaction pooling breaks session advisory locks).

## Consequences

- A restart, maintenance window or bad apply of this instance affects both
  apps. Enabling PITR restarted the instance once.
- A restore is per instance. Recovering one app means a PITR clone, then
  copying that app's databases back. Never restore over the shared instance.
- Local CLIs (`ingest`, `query`, `eval`, `evalsweep`) use the same `NewPool` cap.
  Running one while both apps are at their caps can briefly exceed the
  instance's connection limit. This is an accepted risk; the observed peak is
  far below the caps.
- If `too many clients` errors show up, raising the tier is the escape hatch.
