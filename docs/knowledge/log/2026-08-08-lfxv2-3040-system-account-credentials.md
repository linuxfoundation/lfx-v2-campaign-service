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

**Follow-on (review round 3).** One documentation defect and three tests for boundaries that
had none.

The merge of `origin/main` left live conflict markers in `docs/api-catalog.md`'s account-discovery
row, and the two sides were not a formatting clash: HEAD described the system-account fallback
(404 only when NEITHER the project nor the LF row has a connection; 500 for a fallback onto an
unusable system row), while main described the credentials-first bootstrap that #91 shipped
(`account_id` no longer required, POST-creds → GET accounts → PUT selection, 409 on the toggle and
metrics paths). Both are true of this branch, so taking either side would have deleted a shipped
behaviour from the catalog. The row is now one description covering both, and it ends with the
sentence that makes them compose: a project that has connected NOTHING falls back to the LF row,
while a project that has connected credentials but selected no account is served by its own row
and never falls back — its connection exists, so there is nothing to fall back from.

The three tests all pin `if`s whose two branches are one keyword apart in the source and very far
apart in consequence.

`internal/dispatch/creds_test.go` gains
`TestResolveDoesNotFallBackFromATransientProjectLookupFailure`. The fallback is gated on
`errors.Is(err, domain.ErrNotFound)`; every other repository error must fail closed. Falling back
on a genuine absence spends LF budget for a project that chose to have no account, which is the
design. Falling back on a DB timeout spends it for a project that may have a perfectly good
account of its own, on the strength of a lookup that never answered. The fake serves a USABLE
system row, so the wrong behaviour is the silent, working one — revert-verified by widening the
guard to `err != nil`, which resolves `sys-account` and fails the test.

`internal/infrastructure/postgres/dbtest/connection_live_test.go` is new, and it exists because
`TestClaimVersionIsBackedByACompareAndSwap`'s own doc comment says what changed: "asserted against
the SQL text because this package has no live-database harness in CI". It has one now. The
property under test is not that `AND version = $n` appears in a string — it is that a second
writer holding the same expected version matches ZERO rows once the first commits, that the
repository tells that apart from a missing row, and that the rejected write leaves nothing
partially applied. Only a real `UPDATE` can answer any of the three. A third case pins what
`UpdateWithCredential` is for: the losing rotation carries both a different account and a
different credential, so a two-statement write could leave the row holding one run's account
beside the other's credential — a state that authenticates against the wrong account, which is
the worst available outcome because it is the one that does not fail.

`internal/infrastructure/config/config_test.go` pins the `DATABASE_URL` / `PG*` precedence, and
the second case is the one worth reading: a PARTIAL `PG*` set is REFUSED, not quietly ignored in
favour of `DATABASE_URL`. That is the right answer precisely because the alternative reads as
friendlier — a chart revision that drops `PGPASSWORD` would otherwise redirect the service to
whatever `DATABASE_URL` happens to hold, and everything would start while the data went somewhere
else. The server and `bootstrap-system-account` resolve the DSN through this same function; were
they ever to disagree, the subcommand would install the LF credential in one database and the
server would read from another, and the symptom would be a connection that is simply not there.


## Follow-on (review round 2) — the audience tag had one caller, not four

`systemScoped` re-attributes an unusable-connection defect to the LF system row so an operator
is paged instead of a project being told to go edit a connection it does not own. It was applied
at ONE call site — `resolveGoogleAdsDiscoveryClient` — on the reasoning that tagging at the
caller avoids wrapping the sentinel twice.

That reasoning held for the site it was written at and silently failed for every other one.
`Dispatch` (create) and `resolveGoogleAdsClient` (toggle, metrics) resolve the same connection
through the same validators and returned the identical LF-row defect **untagged**: a project
running on the fallback got a 400 naming a connection it cannot reach, and nobody was paged.

`resolve` itself was fine — it tags what it classifies, and `TestUnusableSystemConnectionKeepsItsOrigin`
covers that. The gap was strictly the defects found AFTER resolve succeeds: inactive status,
undecodable or incomplete credentials, and no account selected. An account-less system row
resolves cleanly and fails in `validateGoogleAdsConnection`, which is exactly the window the
caller-side arrangement left open.

The fix moves the tagging from the callers into the two validators, as a named return plus
`defer func() { err = res.systemScoped(err) }()`, so a return site added later cannot forget it.
`systemScoped` gained an idempotence guard (it returns early when the error already carries
`ErrSystemConnectionNotUsable`), which is what makes "callers can apply it unconditionally" —
what its doc comment always claimed — actually true, and removes the duplicate-prefix objection
that pushed the tagging up to the callers in the first place.

`TestSystemScopedCoversEveryCallerNotJustDiscovery` runs both defect classes across all three
callers, asserting the system row IS tagged and a project's own row is NOT. Reverting either
`defer` fails it in five subtests.

## Follow-on (review round 2) — a deleted connection does fall back

Asked on review: does a soft-deleted project connection now silently run on the LF system
account? It does, and it is intended. `Get` filters `status <> 'deleted'`, producing the same
`domain.ErrNotFound` the fallback keys on, so a delete returns the project to the
never-connected state — the state the fallback exists to serve. The alternative would make
deleting a connection a way to break campaigns rather than a way to disconnect an ad account,
and would make two projects in the same observable state behave differently on history the API
does not expose.

Pinned live, not with a fake: `TestSoftDeletedConnectionIsIndistinguishableFromNoConnection`
creates a connection, deletes it, and asserts `Get` returns `ErrNotFound` while the row survives
with `status = 'deleted'`. An in-memory fake returns `ErrNotFound` by construction and would
pass against a `Get` that had lost its filter. If the product later decides a delete must STOP
dispatch, that test is the one that changes — which makes it a decision rather than a drift.


## Follow-on (review round 3) — the tag had one inspector, not three

Round 2 gave `systemScoped` every CALLER. It did not give the sentinel every INSPECTOR, which
is the same defect one layer up.

`ErrSystemConnectionNotUsable` was matched in exactly one place — the account-discovery
handler. The metrics and toggle handlers matched `ErrAccountNotSelected` and
`ErrConnectionNotUsable`, and both still matched, because `systemScoped` **wraps** rather than
replaces:

    fmt.Errorf("%w: %w", domain.ErrSystemConnectionNotUsable, err)

`errors.Is` therefore continues to report the usability sentinels, so the broad arm won on arm
order alone. A project with no connection of its own, falling back to an unusable LF system
row, was handed a 409 reading *"this project's ad-platform connection is not ready — repair the
connection"*. They have no connection, and the system scope is not addressable by them. Telling
the wrong owner to fix the wrong thing is the ONLY reason the sentinel exists, so on two of its
three consumers the tag was decorative.

Both handlers now inspect it first and return a 500 with an operator-facing `ErrorContext` log,
mirroring the discovery arm. Nothing specific reaches the caller, because there is nothing they
can act on.

Two things make the tests binding. Each asserts the STATUS TYPE, not merely that an error
occurred — the broad arm errors too, so a presence check passes against the bug. And a contrast
test pins that a project-owned connection still gets the actionable 409: without it, hoisting
the system arm to match every unusable connection would satisfy every other assertion while
converting the common, fixable case into an opaque 500.

The general lesson is the one this PR has now paid for twice: a sentinel added for its
AUDIENCE is only worth what its arm order buys. Tagging every producer and inspecting one
consumer leaves the same hole as tagging one producer did.
