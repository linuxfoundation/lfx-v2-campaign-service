# 2026-10-15 — D7 Meta ad read under one ad set

**Creation** — Added `list-meta-ads`
(`GET .../campaigns/{campaign_id}/meta-ad-sets/{ad_set_id}/ads?window=…`), the read-only first
step of decision D7: the ads under one ad set with status, effective status, creative id/name and
impressions/clicks/cost/CTR from one level=ad Insights read. It mirrors the ad-set read —
`campaign_manager`, the same provenance (`metaAdSetScope`), raw-body `identityjson.Check` on every
page and the same error classification — and first proves the ad set is the campaign's under the
connection's account (`GetAdSetState`, 409 otherwise). Every ad's `adset_id`, `campaign_id` and
`account_id` are checked. The listing is capped with a `truncated` flag; delivery is all or
nothing. No writes: per-ad pause/resume awaits decision D7. New concept
[Meta Ad Read Under an Ad Set](../architecture/meta-ads.md); api-catalog row, parity row and chart
comments updated (no rule or route change).
