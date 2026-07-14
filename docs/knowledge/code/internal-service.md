---
type: "Go Package"
title: "internal/service"
description: "Campaign service business logic, including Readyz (DB-backed readiness) and Livez (process-only liveness)."
resource: "internal/service"
---

# internal/service

Package service contains the campaign service business logic, including the
implementation of the generated Goa service interfaces.

`GET /readyz` ANDs an optional `ReadinessChecker` (the PostgreSQL pool when
wired) into readiness with a ~2s timeout and returns `503` when the dependency
is unhealthy. `GET /livez` remains process-only so database outages do not
trigger Kubernetes restarts.

`BriefService` implements brief CRUD and campaign endpoints. Campaign creation
(`CreateCampaigns`) requires an approved brief, rejects empty and duplicate
platform sets (a duplicate would create two paid upstream campaigns), then hands
off to the `Orchestrator`, which persists a job and dispatches per platform
asynchronously (bounded concurrency). Dispatch is idempotent: a brief already
carrying a campaign with an upstream id for a platform is reused rather than
re-created, and a transient existing-campaign fast-path lookup error does NOT
fail the dispatch — it falls through to `ClaimCampaignDispatch`, whose atomic
claim safely either claims-and-dispatches or reuses the conflicting row, so a
duplicate is avoided without failing on the transient error. Replacing a brief's
content resets it to `draft` (re-approval required). Optimistic concurrency is enforced via
version/If-Match (`428` when missing, `412` on mismatch).

Dispatch is durable (LFXV2-2665): single-flight per (brief, platform) is
enforced by an atomic claim — `ClaimCampaignDispatch` does INSERT ... ON CONFLICT
DO NOTHING of a `pending` campaign row, so exactly one worker across replicas
wins the claim (the unique index arbitrates) with no held connection or blocking
lock. A worker that loses the claim reuses the existing row instead of dispatching
again; the pending row also survives an upstream-create-then-crash, making the
orphaned upstream campaign recoverable. The orchestrator tracks in-flight runs
and its `Shutdown` drains them (bounded) before the DB pool closes, and on
startup jobs left non-terminal beyond a staleness cutoff are failed-forward (they
cannot be safely resumed without provider idempotency keys).

See [internal/service](../../../internal/service).
