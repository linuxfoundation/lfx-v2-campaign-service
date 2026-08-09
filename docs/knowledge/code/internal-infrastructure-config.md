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
missing, so a caller can degrade rather than the pod refusing to start. `AI_MODEL` is not a
secret — empty selects `llm.DefaultModel`.

**These three are loaded and chart-wired but not yet READ by anything (LFXV2-2775 part 1 of
2).** Nothing constructs an `llm.Client` today; the consumer — the email-copy step in the
HubSpot dispatch, which will fall back to the cloned template's own body when the group is
unconfigured — lands in part 2. Setting them now is harmless and changes no behaviour.

`String()` prints `AI_PROXY_URL` through `redactAIProxyURL`, which rebuilds it from scheme,
host and path and drops everything else. The field LOOKS secret-free — the key has its own
field — but that is a property of whatever an operator typed, not of the field: userinfo and
the query are both credential-bearing, both survive `%q` intact, and `String()` is the form
every config log line uses. It does not mask wholesale the way `redactDatabaseURL` does, on
`redactNATSURL`'s reasoning: the only question the value answers is "is copy generation
pointed at a proxy, and which one", and `[redacted]` answers neither, while scheme/host/path
answer both and are structurally incapable of carrying userinfo or a query once `url.Parse`
has split them out. Two shapes mask anyway, because no component of them is known safe: an
unparseable value, and an OPAQUE one (`mailto:u:p@host`) whose whole content sits in a field
this does not render — a missing `Host` is the tell. This is redaction for DISPLAY only;
`llm.NewClient` REJECTS both components outright, so a value that reached a live client has
neither.

`splitCSV` parses the comma-separated `EVENT_URL_NAT64_PREFIXES` into its non-empty,
space-trimmed entries, returning nil for an empty or all-blank value so a caller can tell
"not configured" from "configured with nothing" without inspecting elements. Blank entries
are dropped rather than passed through: a trailing comma would otherwise become an empty
string, and the one consumer PANICS on a value it cannot parse.

See [internal/infrastructure/config](../../../internal/infrastructure/config).
