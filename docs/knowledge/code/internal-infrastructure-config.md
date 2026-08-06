---
type: "Go Package"
title: "internal/infrastructure/config"
description: "Application configuration from CLI flags and env vars, including PG* composition into a PostgreSQL DSN and optional SNOWFLAKE_* warehouse credentials."
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

`SNOWFLAKE_ACCOUNT` / `SNOWFLAKE_USER` / `SNOWFLAKE_PRIVATE_KEY` /
`SNOWFLAKE_WAREHOUSE` / `SNOWFLAKE_ROLE` load the read-only warehouse client used
by audience building's past-editions lookup. Unlike PG*, these are optional as a
GROUP and unvalidated at config-load time: when account/user/key are not all
present, `internal/container`'s `newAudienceBuilder` treats the warehouse as
unconfigured and audience building degrades to country-only rather than failing.

See [internal/infrastructure/config](../../../internal/infrastructure/config).
