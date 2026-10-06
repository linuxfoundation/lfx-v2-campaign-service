# 2026-10-05 — Microsoft budget/status PUT: a refusal after a retried 429 is unconfirmed

**Fix** — post-merge review of PR #249 (Copilot, `internal/platform/microsoft/budget.go`).

- **What the loop retries.** `Client.do` (now `doCounted`) retries an idempotent call ONLY on a
  429 — including a 429 whose body was unreadable or oversized — up to `retryMax` times. A 5xx,
  3xx, timeout or transport failure is never retried; it is returned on the attempt that produced
  it and is already unconfirmed.
- **The gap.** A mutating 429 is ambiguous under this package's own contract
  (`createOutcomeAmbiguous`; an exhausted 429 on the budget PUT is already `IsOutcomeUnconfirmed`).
  Yet a 429 followed on retry by a definite 4xx or a PartialError was classified on the last
  attempt alone, so `UpdateCampaignDailyBudget` could return `ErrSharedBudget` /
  `ErrBudgetAmountInvalid` — which the dispatcher maps to the "platform confirmed NO change"
  `ErrBudgetShared` / `ErrBudgetAmountRejected` — even though the first attempt may have applied.
- **The fix.** `doRequestCounted` returns how many attempts were retried; `putUpdate` (shared by
  the budget write and the status toggle) wraps every non-success after at least one retry in
  `retriedUnconfirmedError`, whose `Unconfirmed()` makes `IsOutcomeUnconfirmed` true, so no
  sentinel mapping runs and the dispatcher answers 503 "verify upstream". 429-then-success still
  succeeds; an unretried refusal is classified exactly as before.
- **Tests.** `TestUpdateCampaignDailyBudget_RefusalAfter429IsUnconfirmed` (429 then shared-budget
  PartialError / 4xx, invalid-amount PartialError, plain 4xx → unconfirmed, no sentinel, no amount
  reason), `TestUpdateCampaignDailyBudget_429ThenSuccessSucceeds`, and
  `TestPutStatus_RejectionAfter429IsUnconfirmed`. The
  [internal/platform/microsoft](../code/internal-platform-microsoft.md) and
  [internal/dispatch](../code/internal-dispatch.md) concepts say the same.
