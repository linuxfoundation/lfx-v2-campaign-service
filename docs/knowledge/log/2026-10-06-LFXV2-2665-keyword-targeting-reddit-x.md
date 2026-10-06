# 2026-10-06 — Keyword-targeting levers for Reddit and X

**Update** — the last keyword-lever gap for Reddit and X (LFXV2-2665). On these platforms a
keyword is an entry in the ad group's (Reddit) or line item's (X) TARGETING, not a pausable
criterion, so they get their own read and reduce-only removal rather than new kinds on
`apply-keyword-actions`, which is unchanged for Google Ads and Microsoft Advertising.

- **Research first.** Reddit's create path sets `targeting.keywords` from
  `redditConfig.keywords`; X's create path sets no targeting criteria at all, so X keyword
  targeting exists only where an operator added it. Reddit replaces the targeting object as a
  whole on PATCH (secondary sources — the v3 OpenAPI document cannot be fetched from the
  authoring environment); X deletes one `targeting_criteria` entity per keyword. No per-keyword
  metrics were built: Reddit's `KEYWORD` breakdown sits behind its default-off reporting, and X
  segmentation is async-only with no keyword segmentation found. Findings and citations:
  [Keyword Targeting on Reddit and X](../architecture/keyword-targeting-reddit-x.md).
- **New endpoints**: `GET …/campaigns/{campaign_id}/keyword-targeting` and
  `POST …/campaigns/{campaign_id}/keyword-targeting/removals`, `campaign_manager`, inheriting the
  briefs route and rule (parity rows added).
- **Guards**: ownership of the ad group / line item proven from the platform before any write;
  provenance fails closed on removal; a removal leaving no keyword is refused (409); Reddit's
  write is compare-and-set on a `sha256:` revision of the whole targeting and confirmed by a
  re-read; X reports APPLIED / FAILED / UNCONFIRMED per criterion, never retrying a 429.
- **Default-off Reddit writes** behind `REDDIT_KEYWORD_TARGETING_WRITES_ENABLED` (chart value
  `"false"`, README, constants): the whole-object round trip is unexercised against a live
  account. The read and X's removal are not gated.
