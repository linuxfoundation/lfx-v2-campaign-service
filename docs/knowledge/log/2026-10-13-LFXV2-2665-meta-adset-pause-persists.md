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
