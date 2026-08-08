# 2026-08-08 — LFXV2-3040: system account credentials

**Update** — the LF-owned system ad-account credentials now have a way IN. `internal/bootstrap`
installs and rotates the row for `model.SystemProjectID`, which no HTTP route can reach
(`rejectSystemScope`), driven by the `bootstrap-system-account` subcommand; `resolved` carries
whether credentials came from that fallback so a defect in it pages an operator instead of
returning a 400 to a project that owns no connection to edit. Lessons below.

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

## Whose connection is broken decides who can fix it

A project with no connection of its own runs on the LF system row, and until this round a
defect in that row surfaced as `ErrConnectionNotUsable` alone — the same error a project's
OWN broken connection produces. Discovery answered 400 and told the caller to edit "the
stored connection": they have none, and the system scope is unaddressable by construction
(`rejectSystemScope`), so the one party who could act heard nothing. `ErrSystemConnectionNotUsable`
is wrapped ALONGSIDE the original, so nothing that merely asks "refused before the platform?"
has to learn about it, and it is applied at both places a defect can be found — inside
`resolveConn`, and later by an adapter's own validator, via `resolved.systemScoped`.

**An error's HTTP status answers "what happened"; its ownership answers "who can fix it",
and the two are not the same question.**

## The installer writes past the API, so it must re-check what the API checks

`bootstrap-system-account` speaks to the repository and encryptor directly, which is what
makes it able to reach the reserved scope at all — and also what lets it write values
`design/connection.go` would refuse with a 400. Three separate versions of one mistake:
credential fields were checked for PRESENCE rather than decoded as non-empty STRINGS, so
`"client_id": 123` installed and failed at dispatch; account ids and path-interpolated
config values were not shape-checked at all, so Meta `account_id: "foo"` or an X id
containing `/` landed on an ACTIVE row; and the rotation was TWO writes, so a failed second
one paired a new credential with an old account. Reordering the two only chose which mixed
state a failure left behind, so the port grew `UpdateWithCredential` instead: account,
config and credential go in ONE statement gated on the row's version. A partial write is no
longer reachable, and a concurrent rotation loses the version check — is told nothing was
written and to rerun — rather than interleaving with the winner. The command stays
idempotent, so a re-run converges.

An account-less row is also no longer installable for a provider that cannot finish one.
Credentials-first is a real state only where the dispatcher can enumerate the accounts a
credential reaches, which today means Google Ads alone; the LinkedIn, Meta, Reddit, X and
Microsoft adapters each refuse an empty account id and offer no discovery endpoint, so such
a row would install, report success and fail every dispatch forever. `requireAccountID`
gates it, checking the value about to be WRITTEN so a rotation may still omit the flag.

**A tool that bypasses the API inherits every validation the API was doing for it.**
