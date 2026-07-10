---
type: "Go Package"
title: "internal/container"
description: "Dependency injection: opens the PostgreSQL pool, runs migrations, and wires Readyz to the pool."
resource: "internal/container"
---

# internal/container

Package container provides dependency injection for the application.

When a database URL is configured it validates settings, runs migrations,
opens an instrumented `postgres.Pool`, and wires the services against it: the
connection service (with its repo and the AES-GCM credential encryptor), the
brief service and its async orchestrator (brief/campaign/job repos), and the
campaign/health service so `/readyz` reflects DB connectivity. No platform
dispatchers are registered yet, so campaign creation records jobs but performs
no upstream dispatch (a startup warning notes this). Without a database URL the
health service still starts and the connection and brief routes return typed
`503` responses rather than unmounted `404`s.

See [internal/container](../../../internal/container).
