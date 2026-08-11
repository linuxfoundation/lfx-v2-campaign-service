# 2026-08-10 — Meta's connection bootstrap, and the helper that only checks half of it

**Update** — `MetaAdsConnectionConfig` no longer declares `Required("account_id")`. Only that
one field became optional, so a Meta connection can now be created without knowing which ad
account it will use and have that account chosen afterwards:

```
POST   /projects/{id}/connection-meta-ads          (credentials + page_id, no account_id)
GET    /projects/{id}/connection-meta-ads/accounts (discovery, LFXV2-3062)
PUT    /projects/{id}/connection-meta-ads          (set the chosen account_id)
```

`page_id` stays `Required`. It names a Facebook page the operator already controls — nothing
about the token's reachable-account list resolves it, so relaxing it would just move the same
"you'll find out at dispatch time" problem the Google Ads entry describes onto a field discovery
can never help with.

## Why this doesn't repeat the "ship together or not at all" mistake

The Google Ads bootstrap entry (`2026-08-08-lfxv2-2023-connection-bootstrap.md`) states the rule
this repo learned the hard way: relaxing `account_id` without a discovery endpoint produces a
connection that cannot be finished from inside the API, and the only thing gained is a
half-configured row. Read narrowly, LFXV2-3061 looks like it might repeat that: LFXV2-3062 (the
Meta account-picker endpoint) was still open, CI-green, `REVIEW_REQUIRED`, not merged, at the
time this branched.

The distinction that matters: LFXV2-3062 was built as PR 1 of this same two-ticket sequence,
specifically so the completion path would exist before this PR needed it — not discovered
missing after the fact. And independent of merge order, every connection provider already gets a
generic `PUT /connection-{provider}` from `connectionMethods` (`design/connection.go`), which
lets an operator set `account_id` by hand the moment they have one, whether or not the discovery
endpoint has cleared review yet. So the state this PR creates was always completable from inside
the API; LFXV2-3062 makes completing it self-service instead of requiring someone to already
have the id in hand — which is exactly what Google Ads' own discovery endpoint does, no more.

## The helper covers credential state; it deliberately does not cover the account

`resolveMetaCredentials` replaces three inlined copies of the same credential-state check
(active status, decodable JSON, non-empty access token) at `Dispatch`, `ToggleStatus`, and
`ReadMetrics` — the shape `validateGoogleAdsCredentials` established (Reddit's
`resolveRedditClient` adopted it from there and is the nearest sibling), including the
`defer func() { err = conn.systemScoped(err) }()` so every return path is scoped without needing
to remember it individually. `conn` is a plain local bound once from the resolved connection, not
the named return — the not-usable paths return nil, and `systemScoped` no-ops on a nil receiver,
so closing over the named return would drop the tag from every error that carries it (see
`2026-08-10-lfxv2-3061-meta-system-attribution.md`). It does NOT check `account_id`. That split is deliberate, not an
oversight: `Dispatch` needs both `account_id` and `page_id` to create a campaign, but
`ToggleStatus` and `ReadMetrics` target an existing campaign by id and do not require the account id — they need only the credentials. Folding the account check into the credentials helper would make every caller pay for
Dispatch's extra requirement. `requireMetaAccountID` is the second, separately-called helper —
same wrapping shape as Google Ads' equivalent: `domain.ErrConnectionNotUsable` picks the status,
`domain.ErrAccountNotSelected` supplies the `account_not_selected` reason token, matched ahead of
the general unusable-connection arm in `unusableConnectionReason`.

## The system-account installer is the other surface, and it had its own gate

Dropping `Required("account_id")` relaxes the HTTP surface. `bootstrap-system-account` writes
PAST that surface, straight to the repository, so it carries its own rule —
`accountDiscoveryProviders` in `internal/bootstrap/sysacct.go` — and Meta was excluded from it.
That exclusion was correct at the time, and its comment named this ticket as the gate: discovery
alone is half a lifecycle, and Meta's `Dispatch` still answered an empty account id with a
generic error, so an operator holding an account-less Meta system row was told nothing about what
was missing or where to find it.

`requireMetaAccountID` is that second half, so Meta joins the map here. Leaving it out would have
shipped a branch whose design comment and API catalog both say Meta can be created
credentials-first while the one tool that installs the LF's own credentials refuses to.

The bar for the next provider is now stated as both halves together, because Meta is the only
provider where they ever came apart, and this map is exactly where someone would be tempted to
add a provider on the strength of a discovery endpoint alone.

Two tests used Meta as the exemplar of "no discovery, so `-account-id` is required"
(`TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater`,
`TestClearAccountIDIsRefusedWhereNothingCanSupplyOneLater`). They now use LinkedIn, which has
neither half, and the first gained a Meta case asserting the OPPOSITE — that an account-less Meta
install is accepted and writes an empty `AccountID` for the picker to fill. Verified by mutation:
removing Meta from the map fails that case with the `-account-id` refusal.

## What changed to make `*string` build again

Goa's codegen makes a dropped-from-`Required` attribute a pointer on the generated request-body
type. `internal/service/connection.go`'s `CreateMetaAds`/`UpdateMetaAds` assigned
`cfg.AccountID` directly into the domain model's plain `string` field; both now go through
`strVal(cfg.AccountID)`, the same helper Google Ads' equivalent call sites already use. `PUT` is
a full replace, so omitting `account_id` on update clears a previously chosen one — same
semantics as Google Ads, and the second half of the bootstrap: the caller PUTs back the id
chosen from discovery.

## Why the transport-level test is the one that actually pins this

A service-level test proves the domain model accepts an empty `AccountID`; it cannot prove the
HTTP layer will let one through, because Goa's generated request-body validator runs BEFORE the
handler. If `Required("account_id")` were mistakenly re-added to the design, that validator
would reject the request before the service is ever reached, and a service-level test would keep
passing while the bootstrap silently broke. `TestValidateCreateMetaAds_AccountIDIsOptionalAtTheTransport`
calls `connsrv.ValidateCreateMetaAdsRequestBody` directly — the same pattern
`TestValidateCreateGoogleAds_AccountIDIsOptionalAtTheTransport` already established — with a
second sub-test asserting `page_id` is still required, so the two attributes' presence checks
can't drift together undetected.

## Round: the reason token that four documents cited and nothing emitted

**Kind:** Fix

Copilot, on `internal/dispatch/meta.go`, `docs/api-catalog.md` and `design/connection.go`
simultaneously — three places asserting that a connection parked in the credentials-only state
reports `reason=account_not_selected`.

Verified, and it was worse than the report. A previous round had already corrected these from
"the polled job result names the fault" to "the LOG names it", on the true observation that
`dispatchPlatform` collapses every dispatcher error into `platform campaign creation failed`.
But the log did not name it either: the pre-create branch logged `"error", derr` and nothing
else, and `unusableConnectionReason`'s only callers were `internal/service/brief.go`'s
SYNCHRONOUS handlers — the status toggle and the metrics read. Campaign creation is async and is
the only path that creates a campaign, so on the path the whole bootstrap argument is about, the
classification reached nothing at all.

That is the shape worth naming: **the first correction moved the claim to a place it was also
false, because "not the job result" was verified and "the log" was assumed.** Ruling out one
destination is not evidence for another.

Fixed by emitting it rather than by deleting the promise, because the promise is this PR's
justification: a connection that can be created without an account id is only acceptable if an
operator can tell that state apart from a broken credential. `dispatchPlatform`'s pre-create
branch now carries `"reason", unusableConnectionReason(derr)`. That branch only — the retained
branches describe a provider failure, not a connection, and would log `"unclassified"` on every
record, which reads like a classification was attempted and came back empty.

`docs/api-catalog.md` and `design/connection.go` now say plainly that the polled result is
generic and the detail is a log field, and that the rule for relaxing `Required("account_id")`
asks for a DIAGNOSABLE half-configured connection, not a bespoke API code.

**Regression Guard** — `TestOrchestrator_PreCreateFailureLogsAClassifiedReason` asserts the
`reason` slog ATTRIBUTE, not the rendered line: `err.Error()` happens to contain the sentinel's
own wording, so a text match would have passed against the unclassified version and proved
nothing. It also asserts the job result does NOT carry the token, so the docs saying it is
log-only fail a test if that ever changes. Revert-verified: dropping the attribute gives
`logged reason="", want "account_not_selected"`.

One note on building it. The first fake embedded `error` and got `reason="unclassified"` —
looking exactly like a production defect. The cause was the fake: `internal/dispatch`'s real
`preCreateError` has `Unwrap`, an embedded interface does not, and without it `errors.Is` cannot
reach the sentinels. Had the assertion been weaker the fake would have passed against the fix and
against its absence alike. **A fake that omits `Unwrap` silently disables every `errors.Is` in
the code under test.**

## Round: the fix for the missing reason leaked the cause it replaced

**Kind:** Fix

Copilot, on `internal/service/orchestrator.go` — the gate added by the round above logged
`"reason", unusableConnectionReason(derr)` **and** `"error", derr` on the
`ErrConnectionNotUsable` arm.

Verified, and the cited proof is real and stronger than the report. `internal/dispatch/creds.go`
states the rule in the imperative at the point the 400 is built — the 400 arm "logs a fixed
reason token and nothing else ... Do not 'restore' logging of the cause on the 400 path" —
and `unusableConnectionReason`'s doc comment gives the same rule as its reason for existing:
"the errors themselves cannot be logged: one of these conditions is detected by decoding the
decrypted credential blob, and an `encoding/json` error quotes its input."
`docs/knowledge/code/internal-service.md` had already been written to say neither the cause nor
its text leaves the dispatch layer, "not in the response and not in the log line". The code was
the only thing disagreeing.

The token is logged **instead of** the cause, not alongside it — the substitution is the
mechanism, and adding the cause back dissolves it while leaving every citation of the reason
vocabulary still technically true.

That is the shape worth naming, and it is the second time in this fragment that a fix created
the next finding: **a rule that is only prose gets undone by the next person improving an error
message — and the person most likely to do it is whoever just read the prose and is adding the
field it governs.** The previous round was about a promise nothing emitted; this one is about
emitting it and helpfully bringing along the thing the promise existed to exclude.

**Regression Guard** — `TestOrchestrator_ClassifiedPreCreateFailureLogsNoCause` drives the
failure the way it actually happens: a blob that decrypted and then would not JSON-decode, so
the error text is an `encoding/json` message quoting its input. It sweeps EVERY attribute's
rendered value for a canary rather than asserting one key is absent, because the leak does not
care which key carries it, and checks the message text and the polled job result too.
Revert-verified: restoring `"error", derr` gives
`attribute(s) [error] carry the decrypted-credential canary`.

## Round: suppressing the cause also suppressed the scope

The previous round removed the raw cause from the `ErrConnectionNotUsable` arm of the async
pre-create log, which was right. It took the OWNERSHIP signal with it, which was not.

`domain.ErrSystemConnectionNotUsable` is wrapped ALONGSIDE `ErrConnectionNotUsable`, not instead
of it — `internal/dispatch/creds.go:188-191` does the wrapping and `domain/errors.go` says so in
as many words, so that nothing merely asking "was this refused before the platform?" has to learn
about it. The consequence is that a single broad `errors.Is` arm matches BOTH, and matches first.
So a broken LF fallback row logged identically to one project's misconfigured connection: same
message, same reason token, nothing saying that the failure belongs to a row no project can edit
and that every project without its own connection is failing with it. That distinction is the
entire reason the sentinel exists; `errors.go` calls it the operator's page.

The synchronous handlers had always split the two (`brief.go:577`, `brief.go:914`,
`connection.go:276`), each with the system arm placed ABOVE the broad one and a comment saying
why. The asynchronous path is the only one on which a campaign is ever created, and it was the
one path that did not. The fix mirrors those three rather than inventing a new marker attribute:
the scope is carried by the message, because that is what already pages this operator with this
distinction.

Worth naming as a shape: **when a fix narrows what a log line may say, check what else that line
was saying.** The rule being enforced here was about the CAUSE. The arm that enforced it also
happened to be the only place the SCOPE was distinguished, and a rule stated as "do not log the
error" gives no hint that a second, unrelated signal is riding on the same branch. Both rounds of
this gate were correct about the thing they were looking at.

**Regression Guard** — `TestOrchestrator_SystemConnectionPreCreateFailurePagesTheOperator`, with a
`systemCredsDispatcher` whose error wraps the system sentinel alongside the broad one in the order
`creds.go` produces. It asserts BOTH halves at once: the LF-system wording must appear AND the
canary must not, so a split that reintroduced the cause on the new arm fails the same test that
demands the split. Revert-verified — deleting the system case fails it with `logged message =
"platform dispatch failed before upstream create (claim released)"`.

**Doc-accuracy round, same review.** Four places claimed LinkedIn, Microsoft, Reddit and X "have
neither half" of the credentials-first prerequisite. Verified against the adapters and false:
`validateMicrosoftConnection`, `resolveRedditClient` and `validateTwitterConnection` all already
tag a missing account with `domain.ErrAccountNotSelected`; LinkedIn alone returns a generic error.
All four lack DISCOVERY, which is on its own enough to keep them out, so the conclusion held while
the stated reason did not. Corrected in `design/connection.go`, `internal/bootstrap/sysacct_test.go`,
this bundle's `internal-bootstrap.md` and `docs/api-catalog.md` to say none has both and which half
each is missing — which half matters, because the halves are earned separately: whichever of those
three gains a discovery endpoint first becomes eligible in one change, and LinkedIn needs two.
