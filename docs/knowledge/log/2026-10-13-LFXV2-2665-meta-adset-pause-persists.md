# 2026-10-13 — A deliberate Meta ad-set pause survives a campaign pause/resume (LFXV2-2665)

**Update** — Product decision: a pause of a Meta campaign's recorded ad set made through
`toggle-meta-ad-set-status` now survives `toggle-campaign-status` PAUSED → ACTIVE. The pause is
recorded in the campaign row's existing `result` JSON (`UserPausedAdSetID`, no migration), and a
campaign ACTIVATE on such a row calls the new `meta.Client.ActivateCampaignHoldingAdSet`: ads and
campaign activated, ad set left PAUSED. The record is written before the pause is sent (through a
new `beforeWrite` hook on `MetaAdSetStatusToggler`), so an UNCONFIRMED pause stays on record and
unknown is treated as paused. It is withdrawn when Meta definitely refused the pause, and cleared
only by a confirmed resume through the lever, so an UNCONFIRMED resume keeps it. Hand-added ad
sets and adopted rows record nothing, as before. When the record changes, the lever bumps the row
version and returns the new ETag. `list-meta-ad-sets` gains `deliberately_paused`. This replaces
the open product question in
[Meta Ad-Set Monitor and Pause/Resume](../architecture/meta-ad-sets.md).
- PR #309 review: the toggle-meta-ad-set-status contract no longer promises an ETag "also after a 503" — Goa maps the ETag header on the 200 only, so it now says an error carries no ETag and some 503 paths may still have bumped the version: re-read the campaign before retrying. The pause-marker lifecycle comment now also names withdrawal after a definitely rejected or unsent pause.
- PR #309 review: a `ReplaceCampaign` COMMIT error now carries `domain.ErrWriteOutcomeUnknown`, and the lever reconciles it (one guarded rewrite on a fresh connection, plus a re-read) instead of reading it as "not recorded" — before, a pause record that landed despite its commit error suppressed the pause, leaving the ad set serving while reported deliberately paused; now a possibly-landed record is always followed by the pause, and withdrawal / clear-on-resume are reconciled the same way (live test `TestLiveReplaceCampaignCommitErrorIsMarkedAmbiguous`). The race test's channel waits are bounded and its goroutine is released on cleanup.
- PR #309 review: the meta-ad-sets concept and the api-catalog row no longer say the ETag is available on a 503 — the 200 carries it, an error carries none, and some 503 paths may still have bumped the row, so re-read before retrying.
- PR #309 review: the reconciliation no longer opens a second pooled connection while the claim pins its own (which self-starves on a saturated pool, certain with `pool_max_conns=1`). It redoes the guarded write and the read ON THE CLAIMED CONNECTION; only if that connection is unusable does it release the claim and re-take it with `ClaimCampaignVersion` before writing, each step bounded by `persistResultTimeout`. The settlement and withdrawal reads use the claim's connection too. New live tests on a one-connection pool cover the reconcile path and the fail-safe outcome, and both finish well inside the deadline.
