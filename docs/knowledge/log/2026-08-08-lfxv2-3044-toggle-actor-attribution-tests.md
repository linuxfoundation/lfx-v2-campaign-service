# 2026-08-08 — LFXV2-3044: test coverage for campaign status toggle actor attribution

**Update** — The base change (LFXV2-3038) already records WHO performed a campaign status toggle
(pause/resume) in the `updated_by` column via `ToggleCampaignStatus`. This change adds comprehensive
test coverage to pin that behavior and ensure system-initiated toggles (with no authenticated actor)
correctly record NULL.

**Test cases:**

1. **`TestCampaignActor_ToggleStampsUpdatedByOnly`** — An authenticated toggle records the toggler's
   identity in `updated_by` and does NOT touch `created_by`. The two columns answer different questions
   (who paused/resumed vs. who authorized the spend) and must remain separate so a complete audit trail
   is preserved.

2. **`TestCampaignActor_SystemToggleRecordsNoActor`** — A system-initiated toggle (no authenticated
   principal in context, e.g., a scheduled remediation) records NULL in `updated_by`, not an invented
   actor. NULL means "not recorded", a distinct state from "somebody did this", and is the correct
   outcome for any action with no identifiable human behind it.

**Why this was worth its own change.** The implementation was already present in LFXV2-3038 and
working correctly; the tests were missing. Separate coverage ensures the toggle's actor attribution
is pinned against regressions and the NULL case (system-initiated) is explicitly tested as a legitimate
outcome, not a bug.
