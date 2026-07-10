---
type: "Go Package"
title: "internal/container"
description: "Dependency injection: opens the PostgreSQL pool, runs migrations, and wires Readyz to the pool."
resource: "internal/container"
---

# internal/container

Package container provides dependency injection for the application.

When a database URL is configured it validates settings, runs migrations,
opens an instrumented `postgres.Pool`, and passes the pool to
`NewCampaignService` so `/readyz` reflects DB connectivity. Without a
database URL the health service still starts and connection routes return
typed `503` responses.

See [internal/container](../../../internal/container).
