# 2026-10-05 — Microsoft budget write refuses Accelerated on a Search campaign; "platform unchanged" sentinels restated

**Fix** — PR #249 review, plus one follow-up from merged PR #253.

- **Accelerated is Audience-only, so a Search campaign reporting it is refused.** Microsoft's
  [BudgetLimitType](https://learn.microsoft.com/en-us/advertising/campaign-management-service/budgetlimittype)
  reference says `DailyBudgetAccelerated` "is only available for Audience campaigns that use
  unshared campaign-level budgets", and `GetCampaignBudget` reads with `CampaignType` Search
  explicitly. GUARD 3 of `MicrosoftDispatcher.WriteBudget` used to accept Accelerated and echo it
  back on the PUT; it now fails closed with `ErrBudgetUnwritable` (409, zero PUTs) and a message
  saying Accelerated is Audience-only. `microsoft.UpdateCampaignDailyBudget` accepts
  `DailyBudgetStandard` only and refuses anything else before a request is made.
  `BudgetTypeDailyAccelerated` stays as a named constant so the dispatcher can recognize the
  contradictory read. Tests: the dispatcher's Accelerated success case became
  `TestMicrosoft_WriteBudget_AcceleratedSearchCampaignRefused`; the client's PUT-body test now
  sends Standard, and Accelerated joined its refused-before-any-call table. The `PATCH …/budget`
  row of `docs/api-catalog.md` and the [internal/dispatch](../code/internal-dispatch.md) and
  [internal/platform/microsoft](../code/internal-platform-microsoft.md) concepts now say the same.
- **`ErrBudgetShared` / `ErrBudgetAmountRejected` mean "the platform confirmed no change".**
  Several comments in `internal/service/orchestrator.go` (the `BudgetWriter` rules, the
  re-exported sentinel docs, and `UpdateCampaignBudget`'s classification note) said both were
  raised "before any mutate" / "before anything was written". On Microsoft they can also follow a
  PUT that was SENT and DEFINITELY refused (`CampaignServiceCannotUpdateSharedBudget`,
  `CampaignServiceInvalidDailyBudget`, below-spend), which applied nothing. The comments, the
  `internal/domain/errors.go` sentinel docs, `microsoft.ErrSharedBudget`, the dispatcher's
  post-PUT mapping and the concept files now state that invariant, and keep explicit that an
  UNCONFIRMED outcome is never one of these sentinels.
- **Reddit's non-positive-micros refusal is pinned as client-safe** (merged PR #253's review
  thread on `internal/platform/reddit/budget_update.go`).
  `TestCampaignBudget_InvalidCampaignIDRefusedBeforeAnyRequest` now asserts that
  `BudgetAmountReason` returns the reason, that it states "positive number of micro-units", and
  that it does not name `BudgetMicros`.
