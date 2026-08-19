# 2026-08-18 — LFXV2-3260: Microsoft account provenance, and a poll budget that now bounds what it claims

**Fix** — Two defects on the Microsoft metrics path. The first could produce wrong
numbers silently; the second was a comment promising a guarantee the code did not
provide.

## 1. The metrics read never proved which account created the campaign

`MicrosoftDispatcher.ReadMetrics` resolved a client via `resolveMicrosoftClient`, which
scopes the report to the project's **current** Microsoft ad account, and then queried the
persisted `PlatformCampaignID` against it — without establishing that the campaign was
created there.

Microsoft campaign ids are unique only WITHIN an account, and `UpdateMicrosoftAds` can
re-point a project's connection at a different one. After a re-point the stored id is
addressed against the new account, where it either matches nothing — a false "no
metrics" — or **collides with an unrelated campaign, whose numbers are then rendered as
this campaign's measurement**. That is the failure-as-measurement class this ticket
exists to eliminate, on the one path that produces a wrong number rather than an error.

`ToggleStatus` shared the gap, and it is worse there because the path MUTATES: a
colliding id pauses or activates a campaign the project does not own. Both are now
guarded by one shared helper, `verifyMicrosoftAccountMatch`, so they cannot drift.

### The blob had to learn to carry the account id

The blocker was real: `microsoft.CampaignResult` persisted `AccountLabel` — an operator
display string — but no account **id**. The only `AccountID` writes in that package were
on the create request bodies, not on the result.

It turned out to be a small change rather than a large one, for a reason worth recording:
every result path in `CreateCampaign` — success, partial, and each reconcile branch —
derives from a single `namePartial()` closure. Stamping the field there covers all of
them at once. `CampaignResult.AccountID` (`accountId`) is now set from
`c.account.AccountID`, and `TestMicrosoft_DispatchSuccessMapsResultAndCreds` asserts the
persisted VALUE is the ad account `1234567` and not the MCC parent `customer_id`
`9999999` — a key-presence assertion would have passed against that mis-wiring.

Pre-existing rows are not stranded. `microsoftCreationAccountID` prefers the explicit
field and falls back to the `aid=` query parameter of the `microsoftAdsUrl` the blob has
always carried — the same two-source shape `googleAdsCreationCustomerID` uses with
`customerId`/`ocid`. It preserves that helper's contract exactly: an **absent** id means
"unknown, proceed", and only a present-and-different id is a mismatch. Absence cannot
carry a new meaning here, because it already means "written before the field existed".

This differs deliberately from HubSpot, which fails CLOSED on absent provenance: a bare
HubSpot email id has no recoverable fallback, whereas a Microsoft row's console URL
records the account. So Microsoft has no need for `ErrCampaignProvenanceUnknown`.

LFXV2-3050 recorded `ErrCampaignAccountMismatch` as Google-Ads-only across the six
providers. That was already stale — `hubspot.go` implements it — and Microsoft now makes
three. The remaining ad adapters (LinkedIn, Reddit, X/Twitter, Meta) still do not.

## 2. `reportPollBudget` did not bound the submit

The constant's comment said it "bounds the ENTIRE submit+poll phase". It did not: the
deadline was created at `metrics.go:340`, **inside** `pollReport`, after `submitReport`
had already returned. Submit time was free. A slow submit therefore consumed the caller's
20s `metricsCallTimeout` and surfaced `context deadline exceeded` — the caller's error,
not the retryable `ErrReportNotReady` this path is built to return — with no download
headroom left.

Fixed in the direction the comment claimed, rather than by weakening the comment:
`GetCampaignMetrics` now takes the deadline before `submitReport` and passes it into
`pollReport`. `pollReport` also checks the budget BEFORE its first poll, so a submit that
already exhausted it answers `ErrReportNotReady` without issuing a poll there is no time
to act on. (Writing that pre-poll check was itself forced by the same discipline: the
first version of the new doc comment claimed no poll was issued while the check still sat
after `pollOnce`.)

### The first version of the test passed against the defect

Worth recording, because the test looked correct. It drove
`advancingClock(start, reportPollBudget)` — a clock that advances a full budget on **every
reading**. Under that clock the budget expires no matter where the deadline is taken, so
the test observed "budget expired" rather than "submit was charged against the budget",
and it passed with the fix reverted.

The replacement uses a **step clock**: a fixed instant until the submit handler completes,
`start+reportPollBudget` forever after, so elapsed time is attributable to the submit
alone. It distinguishes the two placements — deadline-before-submit expires immediately
(zero polls), deadline-after-submit yields `start+2*budget` and polls normally — and it
fails against the reverted fix.

The general lesson: a test for *where* a deadline is taken needs a clock that can tell the
placements apart. A monotonically-advancing fake expires everything and proves nothing.

## Mutation results

Every guard was mutated with a COMPILING change and the mutation checked for
observability:

| # | Mutation | Result |
|---|---|---|
| 1 | Drop the `ReadMetrics` provenance guard | **Killed** — sentinel absent, and the token server asserts it was never contacted |
| 2 | Drop the `ToggleStatus` provenance guard | **Killed** — on both PAUSE and ACTIVATE |
| 3 | Treat an ABSENT account id as a mismatch | **Killed** — also broke 5 pre-existing toggle tests, confirming the back-compat contract is load-bearing |
| 4 | Stamp `c.account.CustomerID` (the MCC parent) instead of `AccountID` | **Killed** — caught by asserting the value, not the key |
| 5 | Move the deadline back inside `pollReport` | **SURVIVED** the first test; killed after replacing the advancing clock with a step clock |
| 6 | Remove only the pre-poll budget check | **Killed** — `polls = 1, want 0` |

Mutation 5 is the finding of this round. The mutation was not weakened to accommodate the
test; the test was rebuilt until it could observe the defect.
