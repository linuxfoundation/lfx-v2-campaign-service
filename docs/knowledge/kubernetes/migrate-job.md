---
type: "Kubernetes Resource"
title: "Migrate Job"
description: "ArgoCD PreSync hook Job that runs `campaign-service migrate` before each rollout to apply schema migrations; the server no longer migrates at boot and only verifies the schema."
resource: "charts/lfx-v2-campaign-service/templates/migrate-job.yaml"
---

# Migrate Job

An ArgoCD **PreSync hook Job** (`templates/migrate-job.yaml`) that applies all pending
schema migrations **before** the Deployment rolls. It runs `campaign-service migrate` — a
subcommand of the SAME serving image, not a separate binary, because ko publishes only
`cmd/campaign-service` (the `bootstrap-system-account` installer is the other subcommand run
this way). It is the **single writer of schema**: the server stopped migrating at boot and
now only calls `postgres.VerifySchema` (see the *Deployment* concept and
`internal/infrastructure/postgres/pool.go`).

## Why a PreSync Job rather than boot-time migration

Boot-time migration crash-loops the pod when a migration fails, and — under any rolling
strategy — runs the migration from inside a pod that is also trying to become ready. Moving
it to a PreSync hook makes two things true:

- A failed migration is a **failed Job with logs**, and the hook failure **halts the sync**,
  so the previous ReplicaSet keeps serving instead of being replaced by a pod that cannot
  start.
- Migrations run to completion **before** the new pods roll.

The Job sets `argocd.argoproj.io/hook: PreSync` and
`argocd.argoproj.io/hook-delete-policy: BeforeHookCreation` (the prior Job and its logs
survive until the next deploy), `restartPolicy: Never`, a small `backoffLimit` (rides out a
transient "database not reachable yet", not a bad statement — golang-migrate marks a failed
migration dirty and refuses to replay it), and `migrateJob.activeDeadlineSeconds` to bound a
migration that hangs on a lock. It renders the same `.Values.app.environment` (the `PG*` /
`DATABASE_URL` secret refs) the Deployment does, so DSN resolution is identical; it needs
none of the serving-only env.

## The safety boundary PreSync does NOT provide

PreSync runs the migration while the **previous release's pods are still serving** (the
Deployment sync comes after the hook). So a backward-incompatible migration would break
those still-serving pods for the length of the deploy — `strategy.type: Recreate` does not
help, because it only orders the Deployment's own pod swap during Sync, not the PreSync
migration. Every pending migration must therefore be **N-1-safe**, which is what the
expand/contract rule guarantees (see `internal/infrastructure/postgres/migrations/README.md`
and the *internal-infrastructure-postgres* concept). A genuinely unstageable migration is
NOT covered by this Job alone: it needs explicit old-pod shutdown (scale the Deployment to
zero) or a maintenance window.

## Pinned

`charts/lfx-v2-campaign-service/parity_test.go`
(`TestMigrateJobIsPreSyncHookRunningTheMigrateSubcommand`) asserts the Job is a PreSync hook,
runs `args: ["migrate"]`, and carries the database secret refs it needs to connect.
`cmd/campaign-service/migrate_test.go` pins the subcommand dispatch and its stray-argument
refusal.

See [charts/lfx-v2-campaign-service/templates/migrate-job.yaml](../../../charts/lfx-v2-campaign-service/templates/migrate-job.yaml).
