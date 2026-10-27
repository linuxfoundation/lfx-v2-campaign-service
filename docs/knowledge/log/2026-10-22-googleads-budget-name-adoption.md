# 2026-10-22 — adopt the budget holding a DUPLICATE_NAME instead of dead-ending

**Fix** — a `CampaignBudgetError DUPLICATE_NAME` at `campaignBudgets:mutate` no longer ends a
brief permanently. `internal/platform/googleads/budget_lookup.go` is new, and all five creating
channels (`campaign.go`, `pmax.go`, `display.go`, `demandgen.go`, `video.go`) now call
`reuseBudgetAfterDuplicateName` from that arm.

The dead end was real, not theoretical. The budget name is this client's idempotency key and
`ComposeName` is deterministic per brief, so every later dispatch recomposes the same name and is
refused again. The budget holding it was created `explicitlyShared=false` with no campaign
attached, and that combination is invisible in the Google Ads UI: Shared Library → Budgets lists
only explicitly-shared budgets, there is no campaign row to open it from, and change history is
read-only. The earlier recorded cleanup — "delete the two orphan budgets" — was not executable,
which is what made adoption the fix rather than a convenience.

Reuse is gated on evidence read back from the account, never on the argument: exactly one live
budget of that name, in this customer, with a rebuilt (not echoed) resource name; `period`,
`delivery_method` and `type` all the `DAILY`/`STANDARD`/`STANDARD` shape the create would have
produced, since `campaignBudgetCreate` sends no `Period` at all; a positive amount; an
`explicitly_shared` that is present and false, an omitted field being refused rather than read as
false; `reference_count` 0; and a SECOND, campaign-side query confirming no live campaign
references it, because `reference_count` is documented as campaigns ACTIVELY using the budget and
this client creates every campaign PAUSED. Zero rows is treated as a contradiction of Google's own
refusal and declines the reuse — the one place this lookup deliberately differs from
`FindCampaignByName`, where an absence is a clean licence to create.

An amount differing from the brief is aligned through `UpdateCampaignBudget` and recorded in
`Steps`; a failed alignment fails the create. A declined reuse leaves the previous behaviour
untouched, hedge included — Performance Max's `expansionActionRequiredIfExists` is appended only
on that path, because an accepted reuse has just proved no campaign references the budget.

Covered by `budget_lookup_test.go` (one case per gate, the two server-side filters, the
contradiction, the campaign-side check and the amount alignment), a `CreateCampaign` wiring test
proving the reused resource name reaches `campaigns:mutate`, and
`TestCreatePerformanceMaxCampaign_DuplicateBudgetReusesOrphan` alongside the existing
declined-reuse test.
