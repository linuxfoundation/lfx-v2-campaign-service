# 2026-08-08 — LFXV2-3040: system account credentials

**Green gates say nothing about reachability.** The first cut compiled, vetted, linted and
tested, and could not READ the system credentials. The second could not WRITE them. An installer
is part of the feature, and so is its artifact: a separate `cmd/` binary ko never publishes is an
unavailable installer, so it became a subcommand of the service.

**Absence and uncertainty are different answers.** Only a project with NO connection falls back
to the LF account; a broken one must not, or LF money buys a request the project believed was
billed to itself. Same shape in the installer — only `ErrNotFound` may create. And a choke point
only covers what passes through it: `ListGoogleAdsAccounts` was a SEVENTH connection route the
"all six go through `connection_handler.go`" count missed, because the count came from the
abstraction rather than the routes.

**Valid JSON is not the bar — the bar is what the reader matches.** Untagged structs make
`encoding/json` fall back to a case-insensitive match that cannot bridge an underscore, and
snake_case is what the API documents: a working body encrypted cleanly, decoded to an all-zero
struct, failed at dispatch, installer exit 0. Assert on the decode, not the bytes. Then the
non-secret half bit twice — `Update` rewrites every config column, so *requiring* a key forced
every rotation to re-state it, and *replacing* rather than merging NULLed its siblings. Both
follow from validating what will be WRITTEN rather than what was typed.
