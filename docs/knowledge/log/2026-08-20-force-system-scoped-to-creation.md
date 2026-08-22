# 2026-08-20 — force-system-ads-account: scoped to creation, made reversible

**Fix** — the three questions the 2026-08-19 fragment recorded as "left open deliberately"
are now answered in code. All three turned out to be defects with a correct fix rather than
the design forks they were filed as, and each is mutation-checked.

## The flag could strand a live campaign nobody can stop

`credsSource.resolve` is SHARED: it backs `ToggleStatus` and `ReadMetrics`, not only
`Dispatch`. So turning the flag on changed the credentials used for campaigns created BEFORE
the cutover, and each adapter's provenance guard (`verifyMetaAccountMatch` and its four
siblings) then refused them — the stored creation account differs from the system row. A live,
spending campaign returned 409 on **pause** for as long as the flag was on.

That is the one failure that cannot be fixed forward: the spend has already happened, and the
only remedy is turning the flag back off, which is the opposite of what a cutover flag is for.

Two shapes were on the table — scope the flag to creation, or require a no-pre-existing-campaign
cutover. **Scoping won**, and the deciding evidence was in the codebase rather than in the
trade-off: every adapter already treats the creation account as a HISTORICAL FACT read from the
campaign's persisted result (`metaCreationAccountID` and siblings), so "operations on an existing
campaign use its creation account" is the model already in the code, not a new one. The
alternative needs an operational precondition nothing enforces, and fails silently and
expensively when it is violated.

`resolveExisting` is now a separate entry point beside `resolve`, both delegating to the extracted
`resolveWithFallback`, so exactly ONE function can bypass the fallback. Adapters whose credential
helper serves both kinds of caller (`resolveMetaCredentials`, `resolveLinkedInCredentials`,
`resolveRedditClient`) take a `credsResolver` argument rather than reaching for a fixed method:
the resolver cannot infer whether a campaign exists, but the caller always knows.

The regression test asserts the pause SUCCEEDS **and** that it authenticated with the project's
credentials — checked via the `Authorization: Bearer` header, since this client never puts
`access_token` in the query. Asserting only "no mismatch error" would pass against a fix that
resolved the wrong row and got lucky.

A companion test pins the OTHER half: creation still forces. Without it, a fix that scoped too
aggressively — resolving the project row whenever no campaign is in hand — would disable the
feature while every pausing test stayed green.

## The spec claimed a reversibility the code did not provide

The spec said the rollout was "reversible without a code change". It was not. While the flag is
on, discovery resolves the SYSTEM credential, so every id the picker can offer names an LF-owned
account — and the ordinary connection-update flow persists a selected id onto the project's row.
Turn the flag off afterwards and dispatch resolves the project's credential again while the row
still targets the LF account id. Nothing reconciles that; the connection is silently broken by an
operation the operator believed was a rollback.

Closed at the **persistence boundary** (`rejectForcedSystemAccountWrite`) rather than by filtering
the discovery response, because the id is caller-supplied and nothing obliges it to have come from
the picker — guarding the response leaves the same row writable by hand. Both `createConn` and
`updateConn` are guarded: a connection can be created WITH an account id in one call, so guarding
only the PUT leaves the same write available under a different verb.

Clearing a selection stays allowed. PUT is a full replace, so un-selecting is an ABSENT
`account_id`, and refusing that would trap a connection in whatever state the flag found it in —
the opposite of reversible.

**It answers 400, not 409, and that was forced rather than chosen.** The update endpoints declare
`BadRequest` but not `Conflict` (`design/connection.go`'s `update-` block), and Goa renders an
undeclared error type as a **500** — so the intuitive 409 would have reported an operator policy
as a server fault. Checking the design file before picking the status is what caught it; the
generated code compiles either way.

## A system-config failure reported as "connect your project"

A missing forced-system row carried only `ErrNotFound`, and every classifier checks `ErrNotFound`
FIRST — so callers got a 404 telling them to connect their project when the real fault was that
nobody installed the LF system row. That advice cannot work: the project's own connection is
exactly what forced mode ignores, so connecting one changes nothing, while the operator who could
fix it is never paged. The existing `systemOrigin` wrapper never got consulted, because it is only
read INSIDE arms the `ErrNotFound` arm had already won.

`domain.ErrSystemConnectionMissing` is wrapped **alongside** `ErrNotFound`, not instead of it —
the absence is real, and callers that only ask "was anything found?" must be unaffected. It is
ordered above the `ErrNotFound` arm at all five sites: `classifyDiscoveryError`, both `brief.go`
credential switches, `classifyBriefMetricsErr`, and the async dispatch log in `orchestrator.go`.
It is distinct from `ErrSystemConnectionNotUsable` because the repair differs — install a row
versus fix the row that is there.

The sweep covered all 19 `ErrNotFound` comparisons, not the three cited. Six are genuinely outside
the class and were left alone after checking each: `VerifyClaimedVersion`, `mapBriefErr`,
`mapAudienceErr`, `mapErr`, and the two adoption slot checks all classify CAMPAIGN/BRIEF/AUDIENCE
row lookups, which no credential resolution reaches. Adoption resolves through `resolveOwned` and
was never forced.

## A mutation that survived, and what it exposed

The three ordering fixes were mutation-checked by moving each new arm BELOW `ErrNotFound` — all
died. But deleting the sentinel at the **producer** (`resolveForcedSystem`) left every test green.

The ordering tests constructed the error by hand — `errors.Join(ErrSystemConnectionMissing,
ErrNotFound)` — so they proved the arms were ordered correctly while proving nothing about whether
anything ever produces that shape. The fix was reachable only through a wrap no test asserted;
deleting it would have left all five new arms dead code with a green suite.

The lesson generalises past this branch: when a fix spans a producer and its consumers, a test
that builds the intermediate value by hand tests one half and silently assumes the other. A
producer-binding test (`TestForcedSystemMissingRowCarriesTheSystemMissingSentinel`) now asserts
both that the sentinel is present and that `ErrNotFound` survives beside it, and it kills the
mutation.
