# 2026-11-02 — 445 follow-up: cancelled uploads are refused by the memory budget

**Fix** — `membudget.Budget` granted capacity on its fast path without checking the caller's
context, and a queued grant that raced with cancellation was returned as admitted. A disconnected
upload could therefore still read its body and run `image.Decode` (which cannot be cancelled),
holding its wire charge (up to 112 MiB) and then its pixel charge (up to 80 MiB) of the per-pod
upload budget. Both stages now refuse a caller whose context is
already done, and a grant that lands as the context is cancelled is given back
(`TestBudget_CancelledContextIsRefusedWithCapacityFree`,
`TestBudget_GrantRacingCancellationIsGivenBack`,
`TestBudget_GrantAndCancelBothReadyBeforeWaitIsGivenBack`).

`TestUploadAdmission_ProductionConstantsAdmitConcurrentUploads` now builds the budget as the
container does, with the 80 MiB pixel reserve, and asserts the 15 concurrent small uploads the
wire share admits; it previously used a reserveless budget and counted 25. The hold-and-wait tests
wait for the queue to hold the expected waiters instead of sleeping, and
`TestBudget_ReleaseServesSecondStageFirst` uses sizes where the grant order is observable.

Correction to `2026-11-02-179-one-upload-memory-budget.md`: `TestUploadMemoryBudgetComposes`
asserts at least **15** small uploads on the fallback budget, not 16.
