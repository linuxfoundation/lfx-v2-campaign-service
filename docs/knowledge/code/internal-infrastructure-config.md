---
type: "Go Package"
title: "internal/infrastructure/config"
description: "Application configuration from CLI flags and env vars, including PG* composition into a PostgreSQL DSN."
resource: "internal/infrastructure/config"
---

# internal/infrastructure/config

Package config provides application configuration loaded from CLI flags and
environment variables.

PostgreSQL settings are loaded from `PGHOST` / `PGPORT` / `PGUSER` /
`PGPASSWORD` / `PGDATABASE` / `PGENGINE` and composed into `DatabaseURL`
in-process (so Helm does not interpolate the password). An explicit
`DATABASE_URL` remains supported. Incomplete PG* sets fail validation;
fully empty database config is allowed for metadata-only / unit-test mode.

See [internal/infrastructure/config](../../../internal/infrastructure/config).
