# 2026-08-08 — LFXV2-3044: test coverage for campaign status toggle actor attribution

**Update** — The base change (LFXV2-3038) already records WHO performed a campaign status toggle
(pause/resume) in the `updated_by` column via `ToggleCampaignStatus`. This change adds comprehensive
test coverage to pin that behavior and ensure system-initiated toggles (with no authenticated actor)
correctly record no actor at all.

**Test cases:**

1. **`TestCampaignActor_ToggleStampsUpdatedByOnly`** — An authenticated toggle records the toggler's
   identity in `updated_by` and does NOT touch `created_by`. The two columns answer different questions
   (who paused/resumed vs. who authorized the spend) and must remain separate so a complete audit trail
   is preserved.

2. **`TestCampaignActor_SystemToggleRecordsNoActor`** — A system-initiated toggle (no authenticated
   principal in context, e.g., a scheduled remediation) hands the repo no actor rather than an invented
   one. "Not recorded" is a distinct state from "somebody did this", and is the correct outcome for any
   action with no identifiable human behind it.

   The first version of this entry said such a toggle "records NULL", which is wrong one layer down:
   `replaceCampaignQuery` writes `updated_by=COALESCE($9, updated_by)`, so the nil PRESERVES whoever
   last moved the campaign. Nil is an instruction to record nothing, not to erase. The fixture now
   seeds a prior mover so the test's subject is unambiguous — the service must not substitute that
   prior actor (or any stand-in) for the principal that never acted; keeping the column's old value
   is the repo's decision, made in SQL, and pinned in `campaign_repo_test.go`.

**Why this was worth its own change.** The implementation was already present in LFXV2-3038 and
working correctly; the tests were missing. Separate coverage ensures the toggle's actor attribution
is pinned against regressions and the unattributed case (system-initiated) is explicitly tested as a
legitimate outcome, not a bug.
