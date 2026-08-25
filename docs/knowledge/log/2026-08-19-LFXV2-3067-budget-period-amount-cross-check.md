# 2026-08-19 — LFXV2-3067 budget period and amount were never cross-checked

**Fix** — the settings decoder refused a row carrying BOTH `amount_micros` and
`total_amount_micros`, but nothing checked either amount against `campaign_budget.period`.
`CUSTOM_PERIOD` / `DAILY` appeared in that file only in doc comments, never in a check.

## What decoded cleanly

Both of these were accepted as valid readings of a campaign's budget:

```
amountMicros=500000000       period=CUSTOM_PERIOD
totalAmountMicros=9000000000 period=DAILY
```

Each carries exactly one amount, so the mutual-presence guard never fired. Each is
nonetheless as self-contradictory as a row carrying both: upstream, the amount field is
SELECTED BY the period — `amount_micros` for a daily budget, `total_amount_micros` for a
custom-period one.

## Why it produced a false operator finding

`googleAdsUpstreamBudgetAmount` (dispatch) reads whichever amount is present and **never
consults the period**:

```go
micros := s.BudgetAmountMicros
if micros == nil {
	micros = s.BudgetTotalAmountMicros
}
```

So a `DAILY` row carrying only a total had a whole-flight spend cap rendered as the
upstream budget and compared against a daily recorded amount. The readback reported a
BUDGET DIVERGENCE, and an operator was sent to investigate a spend discrepancy that does
not exist — while the actual defect was field selection. That is precisely the fabricated
finding this readback exists to make impossible; the same reasoning the both-amounts guard
already stated, applied to the case it did not cover.

The two guards stay separate rather than being merged: they reject different row shapes and
name different contradictions, and the both-amounts message is the accurate diagnosis for a
row that carries both regardless of what its period says.

## An absent period PASSES, deliberately

The tempting symmetry — "no period means we cannot verify the pair, so refuse" — is wrong,
and it is the decision worth recording.

Absence already carries a meaning here. Every field on `CampaignSettings` is a pointer
precisely so that "Google did not report this" survives decoding, and a partial settings
read is the ORDINARY case: `campaign_budget` is a separate resource joined onto the
campaign, so its fields can be missing while the campaign's own are present. That contract
is already pinned by `TestGetCampaignSettings_UnreadableFieldIsAbsentNotZero`, which asserts
a row with no `campaignBudget` at all decodes successfully with a nil period. Absence cannot
start signalling "inconsistent pair" when it already means "not reported" — a new case does
not get to borrow a token that is already spoken for.

It is also harmless downstream, which is what makes passing the *safe* choice rather than
merely the compatible one. The dispatcher consults the period only when it is non-nil, so an
absent one produces an `unknown` verdict — the honest reading — rather than a fabricated
divergence. Rejecting it would have converted a benign partial read into a hard error and
lost the campaign's other readable settings with it.

`UNKNOWN` / `UNSPECIFIED` pass for the same reason: a value Google explicitly declined to
name contradicts nothing. Only a period NAMING one of the two real values can conflict with
an amount field.

## Shape of the rejection

A `fmt.Errorf`, matching the mutual-presence and `>1-row` guards rather than the
`transportError` used by the non-numeric-amount arms. The three-way classification does not
fit here: `preSendError` / `transportError` / `apiError` describe whether a request reached
the platform, and this request completed with a 2xx. The row is internally contradictory,
which is a row-integrity refusal — the same class as the id-disagreement and duplicate-key
guards. No upstream VALUE is echoed into either message, only field names and the period.

## Two pre-existing tests agreed with the bug

`TestGoogleAds_ReadSettings_DoesNotWriteBackOntoTheRow` (dispatch) built its fixture as
`amountMicros` beside `period: CUSTOM_PERIOD` — literally one of the two inconsistent pairs,
which only decoded because nothing cross-checked them. The guard failed the test, which is
how it was found. Its subject is the read-only/write-back property, not budget consistency,
so the fixture moved to the consistent `DAILY` pairing; the upstream 750 still diverges from
the recorded 500, so the property under test is exercised exactly as before. Every other
settings fixture in that file was already a consistent pair (the `UNKNOWN`-period one
correctly still passes).

## A pre-existing test agreed with the bug's blind spot

`TestGetCampaignSettings_BothBudgetFieldsRefused` used a fixture with both amounts AND
`period: DAILY`. Once the cross-check existed, that row was caught by EITHER guard, so the
test would have passed with the mutual-presence check deleted and stopped being evidence for
the check it names. Its period is now `UNKNOWN` — a value neither cross-check arm can fire
on — leaving mutual presence as the only thing that can reject it. Confirmed by mutating the
mutual-presence check to a no-op: with `DAILY` the test still passed, with `UNKNOWN` it fails.

## Verification

Every guard confirmed by a COMPILING revert, never by reading it. The mutation kept the
`switch` and both constants referenced so no unused-symbol build break could masquerade as a
failing test:

| Reverted | Tests that failed |
| --- | --- |
| both cross-check arms → fall through | `TestGetCampaignSettings_DailyPeriodWithTotalAmountRefused`, `TestGetCampaignSettings_CustomPeriodWithDailyAmountRefused` |
| mutual-presence check → no-op | `TestGetCampaignSettings_BothBudgetFieldsRefused` (only after its fixture was disambiguated) |

`TestGetCampaignSettings_ConsistentPeriodAmountPairsAccepted` covers the other direction so
the guard cannot over-reject: both consistent pairings, both absent-period cases, and
`UNKNOWN`. It asserts the budget VALUE reached the result rather than merely that no error
came back — a guard that dropped the budget while returning nil would satisfy a bare error
check. It stayed green under the permissive mutation, which is the correct behaviour for a
mutation that only removes rejections.
