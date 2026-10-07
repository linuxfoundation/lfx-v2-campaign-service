# 2026-10-06 — LFXV2-2665 call, promotion and price extensions

**Update** — Added the remaining three ad-extension types this client creates on
Google Search: CALL, PROMOTION and PRICE. They use the same two-mutate shape the
existing three use — an account-level `assets:mutate` followed by a
`campaignAssets:mutate` link against the resource name the create returned — and live
in a new `internal/platform/googleads/assets_extended.go` so `assets.go` keeps holding
only the plan and the two mutates.

What the validators refuse, and why:

- **Call.** Letters in a phone number are refused outright — Google rejects vanity
  numbers such as 1-800-FLOWERS, so accepting one locally would only create an asset
  that can never serve. Numbers de-duplicate on country plus digits-only form.
- **Promotion.** Google models the discount as a oneof and the eligibility condition
  as another. Both are enforced here rather than left to upstream: exactly one of
  `DiscountPercent` / `DiscountAmount`, and at most one of `PromotionCode` /
  `OrdersOverAmount`. A percentage is sent as micros of a FRACTION
  (`percentOffScale = 10_000`, because Google's 1,000,000 means 100%), so 25% is
  250,000 — micros of a percentage would discount by a hundredth of what was asked.
- **Promotion dates.** The serving window and the redemption window are ordered
  INDEPENDENTLY. Nesting the second inside the first would refuse the ordinary case of
  an offer that stays redeemable after the ad stops running.
- **Price.** 3..8 offerings, below which Google will not serve the table; headers
  de-duplicate case-insensitively because Google serves one row per header; and EVERY
  offering carries its own clickable destination, so each is tagged by
  `buildTaggedFinalURL` and bounded by `maxFinalURLBytes` exactly as a sitelink is.
- **Enums.** `occasion`, the price `type`, `priceQualifier` and an offering's `unit`
  are checked for SHAPE only (`^[A-Z][A-Z0-9_]*$`), on the precedent structured-snippet
  headers already set: Google revises these enums, and a local allow-list would refuse
  values Google accepts. A test pins that an unknown-but-well-shaped occasion is taken.

All six extension types stay SEARCH only behind the existing `kind != campaignKindSearch`
fence, so a channel added later refuses them automatically rather than silently
inheriting Search's treatment.

**Named gaps, not oversights.** IMAGE extensions are not supported because the asset
carries bytes: adding one would give the SEARCH create path a network fetch phase it
does not have today, which is a change to this package's "Search preflight is PURE"
property and so belongs in its own commit. LOCATION extensions cannot be created
through this API at all — they are derived from a Business Profile linked to the
account. Both are stated in `assets_extended.go`'s header comment and in
`docs/api-catalog.md`.

**Snapshot.** `config_snapshot` is persisted unencrypted, so `googleAdsSnapshotConfig`
now reduces the promotion destination and every price-offering destination through
`sanitizeSnapshotURL`, and its early return names SIX fields rather than four. The price
copy is explicitly DEEP: a `googleAdsPriceConfig` copied at the top level still shares
its `Offerings` backing array with the caller's config, so sanitizing in place would
strip the path off a URL the create path is about to send. Call extensions are neither
sanitized nor part of the early-return condition set — a phone number is not a URL.
