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
re-created, and a transient existing-campaign lookup error is recorded as a
failure rather than risking a duplicate. Replacing a brief's content resets it to
`draft` (re-approval required). Optimistic concurrency is enforced via
version/If-Match (`428` when missing, `412` on mismatch).

See [internal/service](../../../internal/service).
