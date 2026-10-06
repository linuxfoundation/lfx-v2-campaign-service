# 2026-10-05 — LFXV2-2665 bid writer: PR #264 review fixes

**Fix** — three review findings on `update-campaign-bid`.

- **Reddit: a refusal after a retried 429 is UNCONFIRMED.** `request()` discarded the retry
  history, so a PATCH answered first with a 429 and then with a structured `bid_value` 400 was
  reported as a 400 amount refusal. The 400 also released the lock, yet the first attempt may
  have applied. `UpdateAdGroupBid` now uses `requestCounted`, and any failure after a retried 429
  becomes a `retriedUnconfirmedError` (503, verify upstream) before any amount mapping. Every
  other Reddit caller is unchanged. The Reddit budget write keeps the old classification, which
  is recorded as a known gap.
- **`bid_type` has no Goa default.** With `Default("cpc")` plus `Enum`, the generated CLI
  validated the empty value before applying the default, so `{"bid":2.5}` failed. The design now
  keeps only the `Enum`, which makes the field an optional pointer. The service applies the
  default (nil or empty becomes `cpc`). A test drives the real server decoder with an omitted
  `bid_type`.
- **The Reddit remedy names both switches.** Every created campaign is `BIDLESS` on the
  campaign and the ad group, with CBO on, and the adapter checks the campaign first. The
  operator must therefore switch BOTH the campaign's bid strategy and the ad group to
  `MANUAL_BIDDING`. The design description, api-catalog, concepts and error text now say so.
