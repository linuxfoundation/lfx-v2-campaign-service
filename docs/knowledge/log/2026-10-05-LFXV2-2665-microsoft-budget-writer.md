# 2026-10-05 — LFXV2-2665 budget writer: Microsoft Advertising slice

**Creation** — `update-campaign-budget` now supports Microsoft Advertising, the fourth
platform. `MicrosoftDispatcher.WriteBudget` (`internal/dispatch/microsoft_budget.go`) joins the
Google Ads, LinkedIn and Meta implementations of the optional `BudgetWriter` capability, over a
new `GetCampaignBudget` + `UpdateCampaignDailyBudget` pair in
`internal/platform/microsoft/budget.go`. Reddit and X still answer 400.

## What Microsoft's model decided

- **Budget fields on the campaign, unless a shared Budget owns them.** `DailyBudget` and
  `BudgetType` live on the campaign, so the id addresses the budget — but a `BudgetId` > 0 means a
  shared Budget, refused with `ErrBudgetShared` (409) from the read, and again from Microsoft's own
  `CampaignServiceCannotUpdateSharedBudget` if one is attached between the read and the write.
- **Daily only.** Search campaigns are `DailyBudgetStandard`/`DailyBudgetAccelerated`;
  `LifetimeBudgetStandard` is documented as Audience-only. A lifetime request is the siblings'
  pacing refusal — `ErrBudgetUnwritable`, 409 — raised before any credential is decrypted. The
  reported daily type is sent back on the PUT so the write never re-paces a campaign.
- **Experiment campaigns** inherit their budget and are refused (`ErrBudgetUnwritable`).
- **Provenance fails closed**, stricter than `ToggleStatus`, and uses the toggle's own
  `resolveMicrosoftClient` + `verifyMicrosoftAccountMatch` for the mismatch case.
- **The amount is sent unrounded** (a decimal in the account currency); Microsoft's
  `CampaignServiceInvalidDailyBudget` and below-spend refusals are mapped to
  `ErrBudgetAmountRejected` → 400 with a client-safe sentence (the amount rendered as a plain
  decimal, never `1.5e+06`), so a too-small amount is not answered 503 with a retry invitation.
- **Ambiguity is classified on the mutate only**: 5xx, transport, redirect, exhausted 429 or a
  200 that does not answer `PartialErrors` → unconfirmed (503 "verify upstream"); a coded
  PartialError on the single operation or a definite 4xx → a definite failure.

`putStatus`'s body became `putUpdate`, shared with the budget PUT; status texts are unchanged.
The design description was updated (Microsoft listed; shared-budget and daily-only refusals
named) and the Goa artefacts regenerated; the `PATCH …/budget` row of `docs/api-catalog.md`
names Microsoft and its refusals to match. The `ErrBudgetShared` and `ErrBudgetAmountRejected`
docs (`internal/domain/errors.go`) now state the invariant both sentinels actually carry — the
platform was NOT changed, refused before the mutate or definitely refused by it — since
Microsoft raises each from its mutate as well as (for the shared budget) from its read.

**Not verified live:** the endpoints, field names and error codes come from Microsoft's published
v13 reference, and have not been exercised against a real Microsoft Advertising account. The
numeric codes matched alongside the symbolic names (1100, 1106, 1123, 1159) were checked against
the v13 operation error-code list on 2026-10-05.
