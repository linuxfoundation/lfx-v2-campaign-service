# 2026-11-13 — #586 LinkedIn Text Ads review fixes

**Fix** — pre-PR review findings on the LinkedIn Text Ads branch.

- A stored-asset refusal on a `format: "text"` create now names `linkedInConfig.textAds` (where
  the asset came from) instead of `linkedInConfig.variants`, a field a text create may not carry.
- The campaign-type reuse guard is now pinned at the `CreateCampaign` call site: a same-name
  SPONSORED_UPDATES campaign in the group refuses a text create with the group partial and no
  campaign or creative POST.
- `ValidateImage` and `ValidateTextAdImage` share `decodeImageHeader` (bytes, byte ceiling,
  PNG/JPEG, header format); messages and behaviour are unchanged.
- `PreflightCreate` reuses the bid plan it already resolved instead of re-resolving it and
  discarding the error.
