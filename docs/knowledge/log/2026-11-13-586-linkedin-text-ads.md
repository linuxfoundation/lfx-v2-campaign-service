# 2026-11-13 — #586 LinkedIn Text Ads

**Update** — `linkedInConfig.format: "text"` with `linkedInConfig.textAds` creates a LinkedIn Text
Ad (TEXT_AD) campaign: one DRAFT `content.textAd` creative per entry (headline at most 25
characters, description at most 75, optional 100x100 image of at most 2 MB, landing page the
registration URL with UTMs), no dark post, per LinkedIn's li-lms-2026-02 Text Ads API.

- Text Ads have only manual bidding on LinkedIn, so a text create requires
  `bidding.strategy: "manual"` and is created WEBSITE_CONVERSION / CPC / NONE / unitCost, with a
  `Text` Ad Format name segment; a same-name campaign of the other type is refused, not reused.
- Every defect (format, shape, copy length, image reference, automatic bidding, variants beside
  text ads) is a synchronous 400 before a job exists; omitted or `"single_image"` format is the
  unchanged default create and snapshot.
- Behind `LINKEDIN_ADS_TEXT_ADS_ENABLED`, off by default, until a live probe confirms the
  campaign and creative shapes.
