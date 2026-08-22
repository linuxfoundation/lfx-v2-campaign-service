# 2026-08-20 — a guard that reads a SHARED field, and a fix that skipped the error path

**Fix** — two defects from the review round that the previous commit's own fix triggered. They
are unrelated in code and identical in shape, which is the reason to record them together: this
PR has now produced the same failure three times — *a fix that guards the paid-ads path and
breaks the adjacent one*.

Extends, and does not supersede,
[2026-08-20-force-system-scoped-to-creation](2026-08-20-force-system-scoped-to-creation.md) and
[2026-08-20-existing-campaign-follows-its-creation-account](2026-08-20-existing-campaign-follows-its-creation-account.md).
Both remain correct as written; each recorded a fix, and this fragment records the case that fix
did not cover.

## 1. The reversibility guard was provider-blind, so HubSpot could not be connected at all

`rejectForcedSystemAccountWrite` was added to stop a system-discovered ad account id being
persisted onto a project's connection row while the flag is on. It fired on `c.AccountID`
regardless of provider.

`model.Connection.AccountID` is a **shared field**. `CreateHubspot` and `UpdateHubspot` copy
HubSpot's `account_id` — Required in `design/connection.go`, and naming a *list/audience*, not an
ad account — into that same field. Both verbs funnel through the shared `createConn`/`updateConn`
helpers, so with the flag on **every HubSpot create and update answered 400** and CRM connection
setup was blocked outright.

The blast radius is what makes it worth a fragment rather than a line in a diff. The rejection was
not merely over-broad, it was *unfollowable*: the id being refused could not have come from
ad-account discovery, turning the flag off could not strand it, and the dispatch path gates on
`IsPaidAds()` precisely so email is never redirected (FR-003). There was no action the operator
could take.

The fix passes the provider into the helper — `rejectForcedSystemAccountWrite(c.Provider,
c.AccountID)` — rather than testing it at the two call sites. Both shapes fix today's bug; only
one makes the compiler enforce the pairing, so a provider added later cannot reach the guard
without its classification travelling with the account id. And it asks `IsPaidAds()` rather than
`provider != ProviderHubSpot`, per `Kind()`'s own guidance: an unclassified provider answers
false and is left alone instead of inheriting a paid-ads policy by default.

**The generalisation, which is the part worth keeping:** a guard added for one channel that reads
a field the *model* shares across channels is wrong by default, and the field's name will not warn
you — `AccountID` reads as an ad account right up until you grep who assigns it. Before writing a
guard on a struct field, grep every writer of that field, not every caller of the guard.

## 2. The creation-account rule was applied to the success path and skipped the error path

The previous commit established the invariant: an operation on an existing campaign resolves the
account the campaign RECORDS being created under. It implemented that on the arm where
`resolveWithFallback` **succeeds** and returns a different account, trying the system row before
giving up. On the arm where `resolveWithFallback` **fails**, it returned immediately.

That early return strands a system-created campaign, and two ordinary states reach it:

- the project **disconnects** its own connection. A disconnect is a statement rather than an
  absence, so `systemConn` refuses the ordinary fallback by design and `resolveWithFallback`
  returns `ErrNotFound`.
- the project's row is present but **unusable** — validation or decrypt failure — so
  `resolveConn` returns an error and the fallback never runs at all.

In both, `creationAccountID` already records that the SYSTEM account owns the live campaign, so
the credentials that can address it exist and are reachable. The project's failure said nothing
about that. Pause and metrics therefore failed on a campaign that kept serving and kept spending —
the same no-fix-forward failure the previous two fragments each exist to prevent, arriving through
the error path instead of the success path.

The fix attempts the system resolution and matches it against `creationAccountID` on the failure
arm too, returning the project error only when the system row is unavailable or is not the
creating account.

**The generalisation:** *an invariant is not established until every exit path has been classified
against it.* The docblock stated the rule correctly and the success path implemented it, which is
exactly what makes this shape survive review — the reasoning is present and visibly right, and the
reader checks the arm the prose is standing next to. Enumerate the arms first, then write the fix.

## What the exit-path enumeration produced

Listing `resolveWithFallback`'s arms before touching anything is what turned one reported bug into
two, and turned "handle the disconnect" into a rule:

| arm | before | after |
| --- | --- | --- |
| project row resolves, account matches | return it | unchanged |
| project row resolves, account differs, system matches | return system | unchanged |
| project row resolves, nothing matches | return project (guard renders mismatch) | unchanged |
| project `Get` fails non-`ErrNotFound` (unusable row) | **return error** | try system, match on recorded id |
| project absent + disconnected → fallback refused | **return error** | try system, match on recorded id |
| project absent + no system row | **return error** | error stands (nothing to try) |
| project fails, recorded id EMPTY | return error | error stands, deliberately |
| project fails, system row is not the creating account | — | project error stands |

Two preconditions gate the added lookup, and both are structural rather than conventional:
`IsPaidAds()`, because `resolveForcedSystem` is the LF-system redirect and email must never reach
it; and a non-empty recorded id, because a legacy row makes no system claim and reaching for the
system row on its behalf would address its campaign id in a namespace it does not belong to.
Every caller of `resolveExisting` today is a paid-ads dispatcher — but that is a fact about call
sites, and the whole subject of this fragment is guards that rely on one.

## A mutation that survived, and what it was hiding

Substituting the SYSTEM error for the project error on the both-scopes-failed arm **survived the
entire dispatch suite**. Every other test on that arm asserts a SUCCESS, so nothing observed
*which* of two failures came back.

It is not cosmetic. `internal/service/brief.go` and `internal/service/connection.go` both branch on
`domain.ErrSystemConnectionOrigin` to decide whose repair it is, so the substitution would page the
platform operator for a project that simply disconnected its connection — and the project would
never be told to reconnect. The error a function returns on a failure arm is an operator-routing
decision, and a suite composed entirely of success assertions cannot see it.
`TestSystemRowFaultIsNotReportedAsTheProjectsError` now pins it, and the mutation was re-run after
the test was added rather than assumed dead.

The other mutation worth recording is the mirror of a shape this PR has hit before: neutering the
provider gate open (so the reversibility guard never fires) was killed on **five** paid providers
at once, but only because the new test walks `model.AllProviders()`. The pre-existing guard tests
all use Google Ads, so a fix that widened the exemption past the email channel would have left
five paid providers unprotected with the suite still green — the same "tests pin the mechanism,
not the wiring" shape that let a wiring mutation survive on 4 of 5 platforms on the previous
commit.
