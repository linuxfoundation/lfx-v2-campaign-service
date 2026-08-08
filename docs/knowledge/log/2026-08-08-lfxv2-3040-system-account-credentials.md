# 2026-08-08 — LFXV2-3040: system account credentials

**Green gates say nothing about reachability.** The first cut compiled, vetted, linted and tested,
and could not READ the system credentials; the second could not WRITE them. An installer is part of
the feature, and so are its artifact and its environment — a `cmd/` binary ko never publishes is
unavailable, and one reading `DATABASE_URL` is dead in-cluster, where the chart injects `PG*`.

**Absence and uncertainty are different answers, and a choke point covers only what passes through
it.** Only a project with NO connection falls back to the LF account; a broken one must not. And
`ListGoogleAdsAccounts` was a SEVENTH connection route the "all six go through
`connection_handler.go`" count missed — the count came from the abstraction, not the routes.

**Valid JSON is not the bar — the bar is what the reader matches, and what will be WRITTEN.**
Untagged structs make `encoding/json` fall back to a case-insensitive match that cannot bridge an
underscore, so a body in the documented snake_case decoded to an all-zero struct with the installer
exiting 0. Then `Update` rewrites every config column, so *requiring* a key forced every rotation to
re-state it and *replacing* rather than merging NULLed its siblings. See `code/internal-dispatch.md`.
