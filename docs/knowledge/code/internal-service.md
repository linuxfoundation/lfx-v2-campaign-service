---
type: "Go Package"
title: "internal/service"
description: "Campaign service business logic, including Readyz (DB-backed readiness) and Livez (process-only liveness)."
resource: "internal/service"
---

# internal/service

Package service contains the campaign service business logic, including the
implementation of the generated Goa service interface.

`GET /readyz` ANDs an optional `ReadinessChecker` (the PostgreSQL pool when
wired) into readiness with a ~2s timeout and returns `503` when the dependency
is unhealthy. `GET /livez` remains process-only so database outages do not
trigger Kubernetes restarts.

See [internal/service](../../../internal/service).
