# 2026-10-07 — LFXV2-2665 Meta ad-set read and pause/resume

**Update** — Added ad-set-level monitor-and-optimize for Meta:

- `list-meta-ad-sets` (`GET .../campaigns/{campaign_id}/meta-ad-sets?window=…`): the campaign's
  ad sets with status, effective status, bid strategy, own budget (whole units via the account
  currency offset) and impressions/clicks/cost/CTR from one level=adset Insights read; bounded
  cursor paging, raw-bytes `identityjson.Check` on every page, campaign/account/currency checks,
  explicit-null counters refused, `recorded` marks the row's ad set, `listed: false` for an ad
  set that delivered but is no longer listed. 404 only for a missing row; 100/33 is 503.
- `toggle-meta-ad-set-status` (`POST .../meta-ad-sets/{ad_set_id}/status`): If-Match like the
  campaign toggle, provenance and ad-set ownership proven before ONE unretried write, adopted
  ACTIVATE refused, outcomes APPLIED / UNCONFIRMED / ALREADY_IN_STATE; nothing persisted, ETag
  returned unchanged.
- New `MetaAdSetReader` / `MetaAdSetStatusToggler` capabilities (Meta only), domain sentinels,
  chart comments and parity rows, api-catalog rows, and the
  [Meta Ad-Set Monitor and Pause/Resume](../architecture/meta-ad-sets.md) concept.
