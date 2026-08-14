# 2026-08-14 — Migrations moved from boot into an ArgoCD PreSync Job

**Creation** — `cmd/campaign-service/migrate.go`, `cmd/campaign-service/sysacct.go`,
`internal/infrastructure/postgres/pool.go`, `internal/container/container.go`,
`charts/lfx-v2-campaign-service/templates/migrate-job.yaml`,
`charts/lfx-v2-campaign-service/templates/deployment.yaml`, `values.yaml`,
`parity_test.go`, and the knowledge bundle (linuxfoundation/lfx-self-serve#1543). Follow-up
from @bramwelt's PR #126 review.

## What changed

Schema migrations no longer run at pod boot. They run in an ArgoCD **PreSync hook Job**
(`campaign-service migrate`, a subcommand of the serving image) BEFORE each rollout. The
server now only VERIFIES the schema at boot (`postgres.VerifySchema`, the same
required/invalid-index guard `Migrate` used to run after `Up()`), and never mutates it.

- A failed migration is a failed Job with logs and halts the sync — the previous ReplicaSet
  keeps serving — instead of crash-looping a new pod.
- `initDatabase` swaps `Migrate` → `VerifySchema`; `migrateMu` is gone (verification is
  read-only and idempotent, so overlapping boot retries are harmless). Permanent-error
  classification is unchanged — `IsPermanentMigrationErr` already covered the index sentinels.

## Why a subcommand, not a `cmd/migrate` binary

ko publishes only `cmd/campaign-service`, so a separate binary would have no artifact for the
Job to run — the same reason `bootstrap-system-account` is a subcommand. `migrate` joins it;
`runCommand` now switches on the command name.

## The safety boundary this does NOT remove

PreSync runs the migration while the previous release's pods are STILL serving (the
Deployment sync is later). So `Recreate` does not cover a backward-incompatible migration —
it only orders the Deployment's own pod swap, not the PreSync timing. Every pending migration
must be N-1-safe, which is exactly what the expand/contract rule (#1542) guarantees; a
genuinely unstageable migration needs explicit old-pod shutdown (scale to zero) or a
maintenance window. This is the review insight from #128, now realized: moving migrations to
PreSync does not by itself make an unstageable change safe.

## Recreate + Replace=true are now dead weight, kept one release

With migrations out of boot, the `Recreate` strategy and the `Replace=true` annotation are no
longer load-bearing (the cutover hazard they existed to prevent is gone). They are retained
here only so #1543 and #1544 stay independently verifiable; #1544 returns the Deployment to
RollingUpdate and drops the annotation. The chart comment and the deployment concept now say
so.

## Docs

New `docs/knowledge/kubernetes/migrate-job.md` concept (+ index bullet). The deployment,
internal-container, and internal-infrastructure-postgres concepts and the migrations README
were corrected off their "migrations run at boot" framing. `internal-container`'s frontmatter
description changed ("runs migrations" → "verifies the schema"), so its `code/index.md` bullet
was updated verbatim to match.

## Release

Highest-risk change: new chart version + image, promote dev → staging → prod with the
pending-migration and failure-injection checks the ticket lists at each gate.
