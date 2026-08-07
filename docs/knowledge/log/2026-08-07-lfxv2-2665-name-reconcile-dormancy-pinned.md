# 2026-08-07 — LFXV2-2665: the by-name reconcile is dormant, and now pinned as such

**Update** — Copilot flagged that `meta.CampaignInput.ReconcileByName` is never set by any
production caller, so the by-name reconcile added in this branch closes nothing today.
That is correct and deliberate — `internal/dispatch/meta.go` builds `CampaignInput` without
the field, and the one path that would want the lookup (a retry after an ambiguous create)
never re-dispatches: the orchestrator retains the partial row and answers "reconciliation
required". The reasoning was already written down beside the field and beside the gate; what
was missing was anything that ENFORCED it.

**Fix** — `TestMeta_DispatchNeverOptsIntoNameReconciliation` drives the real dispatcher
against an httptest Meta API that counts by-name lookup requests, and asserts zero. The
existing `TestCreateCampaignWithoutReconcileByNameDoesNotLookUpOrReuse` proves the client's
gate holds when the flag is unset; it says nothing about what the only production caller
passes, which is where dormancy is actually decided. Verified binding by setting
`ReconcileByName: true` in `internal/dispatch/meta.go`: the test fails with the lookup count
and the reason the reuse would be wrong (the campaign name is event/region/objective/project
only, so it is not brief-unique, and reuse would defeat the documented delete → re-dispatch
flow by silently re-running a budget that flow exists to correct).

**Fix** — The PR description previously opened by saying the duplicate-create window is
closed. It now says the mechanism ships opt-in and dormant, with the flag's owner
(LFXV2-2665's reconcile path, which knows it is resuming one dispatch generation) named
explicitly, so nobody reads the merge as having removed the duplicate-paid-campaign risk.
