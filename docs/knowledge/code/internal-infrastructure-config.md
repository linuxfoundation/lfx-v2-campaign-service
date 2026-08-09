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

`AI_PROXY_URL` / `AI_API_KEY` / `AI_MODEL` configure the LF LiteLLM proxy used to generate
email copy. Optional as a GROUP on the same reasoning as `SNOWFLAKE_*` and likewise unvalidated
here: `internal/platform/llm`'s `NewClient` returns `ErrNotConfigured` when url or key is
missing, so the caller degrades to the cloned template's own body rather than the pod refusing
to start. `AI_MODEL` is not a secret — empty selects `llm.DefaultModel`.

`splitCSV` parses the comma-separated `EVENT_URL_NAT64_PREFIXES` into its non-empty,
space-trimmed entries, returning nil for an empty or all-blank value so a caller can tell
"not configured" from "configured with nothing" without inspecting elements. Blank entries
are dropped rather than passed through: a trailing comma would otherwise become an empty
string, and the one consumer PANICS on a value it cannot parse.

See [internal/infrastructure/config](../../../internal/infrastructure/config).
