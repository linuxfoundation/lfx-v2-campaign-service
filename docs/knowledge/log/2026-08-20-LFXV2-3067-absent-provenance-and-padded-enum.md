# 2026-08-20 — LFXV2-3067 absence read as a match, twice, at two layers

**Fix** — two review findings on the settings readback that turn out to be the same defect
wearing different clothes: a value that is not there, or not exactly what was expected, taking
the SAFE branch of a comparison it never actually satisfied.

## An unrecorded creating customer was treated as a matching one

`GoogleAdsDispatcher.ReadSettings` guarded account identity with

```go
if created := googleAdsCreationCustomerID(campaign); created != "" && created != client.CustomerID() {
```

The `created != ""` conjunct means an EMPTY provenance falls through to the read. But an empty
provenance is not agreement — it is the absence of any statement at all. The stored
`PlatformCampaignID` is a bare numeric unique only WITHIN a customer, so the campaign was then
queried under whatever account the project currently resolves to; on an id collision the search
returns ANOTHER account's campaign, and this endpoint reports a divergence between this
campaign's recorded budget and a different campaign's actual one. That is exactly the false
finding a readback exists to make impossible.

The in-code comment defending the fallthrough argued it was a shared service-wide convention
and that the risk was bounded because the endpoint never writes back. Both halves are true and
neither is sufficient. The endpoint's entire output IS a comparison, so "only" emitting a
confidently wrong report about someone else's account is the whole failure mode, not a
mitigation of it. And the convention argument had a measurable consequence the comment did not
notice: Google Ads is the ONLY implementor of `SettingsReader`, so the handler's
`ErrCampaignProvenanceUnknown` arm in `brief.go` — a documented 409 with its own log line and
its own operator remedy — **was unreachable**. A documented error nothing can produce.

Fixed by splitting the guard into two arms, both load-bearing:

```go
created := googleAdsCreationCustomerID(campaign)
if created == "" { /* Join(ErrCampaignProvenanceUnknown, ErrCampaignAccountMismatch) */ }
if created != client.CustomerID() { /* ErrCampaignAccountMismatch, retained unchanged */ }
```

Joined rather than returned alone so existing `errors.Is(err, ErrCampaignAccountMismatch)`
callers keep matching, which is the contract `domain/errors.go` already states. This mirrors
what the HubSpot metrics read does for an unrecorded portal, including checking it BEFORE the
client is consulted: absent provenance is a purely LOCAL fact and no upstream answer changes it.

**The existing tests asserted the broken behaviour**, and not by intent. Twenty-three of the
twenty-five `ReadSettings` fixtures built a `model.Campaign` with no `Result` blob at all — so
every test of budget formatting, flight dates, channel types and absent-flag handling was
reaching the code under test only BECAUSE absent provenance was waved through. They went red
on the fix. The right repair was to give them provenance (`settingsProvenance()`), not to
soften the guard: each test's SUBJECT is a different concern, and a fixture that silently
depends on an unrelated guard being open is a test that will mislead the next person too.

## A padded enum slipped past both budget refusals

Same shape one layer down, in `campaign_settings.go`. The period/amount cross-check compared
with exact equality against `budgetPeriodDaily` / `budgetPeriodCustom`, while `blankToNil`
returns non-blank strings VERBATIM. So `" DAILY "` beside a `total_amount_micros` matched
neither `case` and decoded cleanly — the exact contradictory pair the guard was added to refuse.
`googleAdsUpstreamBudgetAmount` then reads whichever amount is present without consulting the
period, comparing a whole-flight cap against a daily recorded budget.

Fixed by `strings.TrimSpace` on the switch subject ONLY. The reviewer flagged the tension with
a sibling finding that trimming an opaque identifier is wrong, and the distinction is the
value's KIND rather than the operation:

* `period` is a CLOSED-SET ENUM. Recognising `" DAILY "` as `DAILY` DISCOVERS what the value
  already unambiguously is. The trimmed string chooses a REFUSAL and is never written back onto
  `settings` — the caller still receives the verbatim bytes.
* An IDENTIFIER or a strictly-parsed date has no closed set to recognise against, so trimming
  INVENTS a well-formed value the platform never sent, manufacturing agreement.

The direction of the manufactured outcome is the test: normalising toward a REFUSAL can only
ever refuse more, while normalising toward a COMPARED VALUE can fabricate a `match`. That is
why `googleAdsBudgetTypeFromPeriod` must keep NOT trimming — the 2026-08-19 fragment records it
being un-trimmed for precisely that reason, and it produces a compared value, so a padded period
there must keep yielding `unknown`. Two functions in the same flow, opposite correct answers,
decided by what the normalised value is USED for.

## Mutation-verified

Each guard was reverted in a form that still COMPILES, since a build break proves nothing:

```
restore `created != "" && created != ...`   -> AbsentProvenanceIsRefusedBeforeContact FAILS (all 4 cases)
neuter the retained mismatch arm            -> AccountMismatchIsRefusedBeforeContact FAILS
drop TrimSpace from the switch subject      -> PaddedPeriodWithContradictingAmountRefused FAILS (all 3 cases)
```

The second one matters as much as the first: a fix that adds an absence arm can quietly swallow
the mismatch arm it was supposed to preserve, and only mutating the arm you did NOT change shows
whether it is still bound.
