# 2026-08-24 — the period decides which amount, and absence decides none

**Fix** — settings readback: `googleAdsUpstreamBudgetAmount` selected the upstream budget by
FIELD PRESENCE — `amount_micros`, else `total_amount_micros` — and never consulted
`BudgetPeriod`. The two fields are different quantities, so "whichever one is present" is not a
reading of the budget but a guess at which question the number answers.

## The false match

A campaign recorded as a **daily 500** against an upstream row carrying only
`total_amount_micros=500000000` and **no period** reported:

> `budget_amount: match`

A 500/day rate and a 500 whole-flight cap are not the same budget. The equal digits were a
coincidence of the units, and the readback reported agreement between a configuration and a
platform that may well contradict it — the exact fabricated verdict this endpoint exists to
make impossible.

## The client was already relying on the fix that did not exist

`GetCampaignSettings` refuses an amount that contradicts a **named** period, and deliberately
lets an **absent** one through. That permissiveness is correct and load-bearing: absence means
"Google did not report this field" across all of `CampaignSettings`, pinned by
`TestGetCampaignSettings_UnreadableFieldIsAbsentNotZero`, and it cannot start signalling
"inconsistent pair" without breaking that meaning for every other optional field.

Its comment justified passing the partial pair on by stating what the dispatcher would do:

> the dispatcher consults the period only when it is non-nil and an absent one yields an
> `unknown` verdict rather than a fabricated divergence

That was true of `budget_type` and false of `budget_amount`. **A stated contract is not a
verified one** — the sentence read as a description of existing behaviour, and no test bound
the half it described. The gate is what makes it true.

## The wire format is why absence is not "unknown"

Google Ads REST is protobuf JSON, which **omits default scalars**, so an absent
`campaign_budget.period` is not a distinct "unresolvable" signal — it is simply a field not
returned, which is also what an unreadable budget resource looks like. A guard that treated
absence as a NEW meaning would have contradicted the absence convention this client is built
on. The fix does the opposite: absence selects no amount at all, which is the same answer the
existing `unknown` verdict already gives.

## Shape of the fix

The gate reuses `googleAdsBudgetTypeFromPeriod` rather than re-testing the two enum spellings,
so one period cannot name a budget type for that field while failing to select an amount for
this one. It inherits that function's fail-closed default, which already covers
`UNKNOWN`/`UNSPECIFIED` and the padded `" DAILY "` that `blankToNil` leaves malformed on
purpose.

## Test note

The fixture helper `settingsRow` always renders a `period`, so it cannot build the body that
provokes this. The regressions use raw JSON literals for that reason — **a helper that always
emits a field cannot exercise the field's absence**, and a suite built only from it would have
been testing a body Google never sends.
