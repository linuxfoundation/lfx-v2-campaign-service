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

`String()` prints `AI_PROXY_URL` through `redactAIProxyURL`, which rebuilds it from scheme
and host and drops everything else — userinfo, path, query, fragment. The field LOOKS
secret-free — the key has its own field — but that is a property of whatever an operator
typed, not of the field: a URL has several places a credential rides for free, all of them
survive `%q` intact, and `String()` is the form every config log line uses. It does not mask
wholesale the way `redactDatabaseURL` does, on `redactNATSURL`'s reasoning: the only question
the value answers is "is copy generation pointed at a proxy, and which one", `[redacted]`
answers neither, and scheme and host answer both.

**The path took three rounds to drop, and the reason is the interesting part.** Each earlier
version reasoned that `url.Parse` had already split the dangerous components into their own
fields, so whatever remained was structurally safe. That confuses where the DELIMITERS fall
with what a component CONTAINS — the same mistake made about the scheme
(`localhost:sk-secret` parses the secret as an opaque scheme), then about the host, and
finally about the path (`https://litellm.example.com/sup3r-s3cret/v1` parses perfectly).
The rule that survives all three is narrower than "not userinfo and not query": a component
is reproduced only when it is BOTH structurally incapable of holding a secret AND load-bearing
for the diagnosis. The path fails on both counts — "which proxy" is answered by the host.

Two shapes mask anyway, because no component of them is known safe: an unparseable value, and
an OPAQUE one (`mailto:u:p@host`) whose whole content sits in a field this does not render — a
missing `Host` is the tell. A scheme that is neither `http` nor `https` masks for the same
reason. This is redaction for DISPLAY only; `llm.NewClient` REJECTS userinfo, query and
fragment outright, so a value that reached a live client has none of them — but it ACCEPTS a
path, and `String()` runs on the startup log path before the constructor in any case.

`splitCSV` parses the comma-separated `EVENT_URL_NAT64_PREFIXES` into its non-empty,
space-trimmed entries, returning nil for an empty or all-blank value so a caller can tell
"not configured" from "configured with nothing" without inspecting elements. Blank entries
are dropped rather than passed through: a trailing comma would otherwise become an empty
string, and the one consumer PANICS on a value it cannot parse.

See [internal/infrastructure/config](../../../internal/infrastructure/config).
