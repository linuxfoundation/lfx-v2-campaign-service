---
type: "Go Package"
title: "internal/platform/snowflake"
description: "Read-only Snowflake client for the email channel: resolves past-edition EVENT_NAME strings from PLATINUM_LFX_ONE for HubSpot BEHAVIORAL_EVENT filters."
resource: "internal/platform/snowflake"
tags:
  - platform-client
  - snowflake
  - email
  - read-only
  - go-package
timestamp: "2026-07-20T00:00:00Z"
---

# internal/platform/snowflake

Package snowflake is a READ-ONLY Snowflake client for the email channel (LFXV2-2772,
epic LFXV2-2770). Its sole job is to resolve the exact past-edition `EVENT_NAME`
strings a HubSpot `BEHAVIORAL_EVENT` audience filter needs — the audience-builder
(LFXV2-2774) uses those verbatim strings as filter values. Configuration is injected
via `NewClient`; the package never reads the environment.

## Read-only by construction

There is NO arbitrary-SQL entry point (unlike the reference app's
`snowflake_query(sql)`). The only method, `ResolvePastEventNames(eventTerm,
locationTerm, currentYear)`, builds a FIXED, fully-parameterized `SELECT DISTINCT
EVENT_NAME, EVENT_ID` against `ANALYTICS.PLATINUM_LFX_ONE.event_registrations`:
caller terms bind as `ILIKE ? ESCAPE '\\'` / `NOT ILIKE ? ESCAPE '\\'` parameters
(never interpolated into the SQL text). Each bind pattern escapes the ILIKE
metacharacters (`\`, `%`, `_`) so a literal `%`/`_` in a term matches literally
instead of acting as a wildcard — the ESCAPE literal is `'\\'` (two backslashes)
because Snowflake parses it by single-quoted-string rules where `\\` is one
backslash. `currentYear` is REQUIRED as a 4-digit year (it's the "past editions only"
guarantee, so a blank/malformed value is rejected rather than silently dropping the
exclusion). The query fetches `maxEventRows+1` so truncation is DETECTABLE: if more
than the cap match, it FAILS CLOSED ("narrow the search term") rather than silently
returning a partial (incomplete) audience. The database/schema/table are package
constants; a defensive `ident` guard neutralizes any future config-sourced identifier
so it can never inject SQL. So the package is structurally incapable of a write or an
injection.

**Source = PLATINUM, not Silver.** Per the email-channel design, the broker resolves
event names from the curated `PLATINUM_LFX_ONE.event_registrations` (the reference
app used `Silver_Segment.EVENT_REGISTRATIONS`). Verified PLATINUM carries
`EVENT_NAME`/`EVENT_ID`.

**Fail-closed.** A query error, or zero rows, is surfaced as an error/empty — callers
MUST NOT substitute guessed/remembered event names (a mismatched string silently
miscounts the HubSpot list), which is why the resolve is the single source of truth.

## Auth + connection

Key-pair (JWT) auth: the injected unencrypted PKCS8 RSA private key
(`Config.PrivateKeyPEM`) signs the Snowflake JWT (`gosnowflake` `AuthTypeJwt`).
`parsePrivateKey` tolerates the common `.env`-injection mangling (wrapping quotes,
literal `\n`/`\r\n` escapes, CRLF) since the key often arrives via an env-injected
secret. The `*sql.DB` pool opens lazily on the first query (so an unreachable
warehouse doesn't wedge `NewClient`), guarded by a mutex so concurrent first queries
can't double-open or race `Close`, and bounded by a per-query timeout. The DSN/config
are never quoted into error messages. The client does NOT retain the injected
`Config` after construction: `NewClient` builds the DSN and drops the PEM, so the
credential isn't held in two places (the built DSN necessarily embeds the signing key
— unavoidable, since the driver needs it to connect — so the DSN is itself treated as
secret).

## Dependency

This package adds `github.com/snowflakedb/gosnowflake` — the only official Go
Snowflake driver. No shared Go Snowflake service exists in the platform (the LFX One
UI backend has a TypeScript Snowflake service, not reusable from Go), so the broker
owns its own connection, consistent with every other platform port owning its client.

## Scope

Read-only event-name resolution. Consumer: the audience-building logic (LFXV2-2774).
