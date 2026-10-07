# 2026-10-05 — Demand Gen ad creation (LFXV2-2665)

**Creation** — a Demand Gen campaign can now be created with an ad, closing the
last creation-side gap on that channel. Before this it was a shell — budget,
campaign, ad group, geo, and no ad — so it could not serve even once a human
enabled it, and the closing step told the operator to go and upload images in the
Google Ads UI.

`internal/platform/googleads/demandgen_creative.go` adds `DemandGenCreative` on
`CampaignInput`: five image-URL lists (marketing, square, portrait, tall portrait,
logo), 1–5 headlines, 1–5 descriptions, a required business name and an optional
call to action. It is the **mirror** of every Search-only field already in this
package — supplied on a Search campaign it is REFUSED, not ignored, exactly as
extensions and proximity targeting are refused on Demand Gen.

The file is split along the preflight boundary, and that split is the whole
orphan-avoidance argument. `preflightCampaignKind` is pure — no network, no `ctx`
— because everything must be decidable before the first budget mutate spends
money. Image bytes cannot be validated without fetching them, so the
locally-decidable half (counts, copy widths, URL shape) stays in the preflight and
`fetchDemandGenImages(ctx, plan)` runs in the cascade but still **before** the
budget mutate. A bad image therefore fails with `result == nil` and nothing
created.

Google Ads takes image assets as base64 bytes, not URLs, so this is the one place
the service fetches a caller-controlled address. That transport is hardened
separately from the API client: https only, no credentials sent, no redirects
followed, a `checkPublicIP` dial guard refusing loopback/RFC1918/link-local
(including `169.254.169.254`)/CGNAT/multicast/ULA and their IPv4-mapped IPv6
spellings, a 5 MiB body cap that binds whether or not a length was declared, and
an allowlist that IS the decoder set — only PNG, JPEG and GIF are imported, so
there is no separate list to drift. Geometry is checked against the decoded image
against Google's documented ratios and minima within ±1%.

Two deliberate differences from the Responsive Search Ad path. The counts are
**not** the RSA counts (1–5 versus 3–15 and 2–4) even though the display widths
coincide at 30 and 90, so a dedicated test asserts the constants differ rather
than relying on a width check that would pass with `maxHeadlines` wired in by
mistake. And over-long copy is **refused, not truncated**: an RSA composes copy
this service generated, while a Demand Gen ad is built from exactly what the
caller named, and quietly shortening it publishes something nobody wrote.

The ad is created PAUSED like every other resource here, which also lets the
existing toggle cascade enable it with no further change.
`CampaignResult` gained `creativeAssetIds` beside `adId`, and the partial-result
contract covers both — an ad create that fails after the assets uploaded still
returns a non-nil result carrying the asset ids, so a retry does not lose them.

`internal/dispatch/googleads.go` carries the config block through with a mapper
that validates nothing, and `googleAdsSnapshotConfig` deep-copies the creative and
reduces all five URL lists through the new `sanitizeSnapshotURLs`. `config_snapshot`
is persisted unencrypted and a CDN asset link often carries its credential in the
query string; the deep copy matters more here than for sitelinks because the image
fetch runs after the snapshot is taken, so sanitizing in place would leave the
create path downloading a URL with its credentials stripped.

Two fixes promised on PR #239 ride along. `checkStatusMutateResults` now matches
a status mutate's results to the operations sent **by resource name, as a set**,
rather than by count: three results for three operations satisfied a count check
even when all three named the same ad group and two of the resources the caller
asked about were never touched. It still accepts MORE results than operations
without complaint, and an unrecognized extra name is ignored rather than fatal —
refusing a toggle that actually applied is the over-refusal this guard must not
commit. The existing fixtures echoed placeholder names (`"ok"`, and a
`customers/123/...` that did not match the connection's account), which the
name-based check correctly read as unaccounted-for operations; they now echo the
resource names the client actually addressed. Separately, `docs/api-catalog.md`
referred to a `campaignType` field in two places; the field is `channel`.
