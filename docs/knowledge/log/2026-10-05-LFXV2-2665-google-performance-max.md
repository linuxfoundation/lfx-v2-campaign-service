# 2026-10-05 — Performance Max campaign creation (LFXV2-2665)

**Update** — Google Ads gained a third channel. `channel: "performance-max"` now
creates a real Performance Max campaign: budget, a `PERFORMANCE_MAX` shell, campaign-level
geo and criteria, and an asset group.

Performance Max has **no ad groups and no ads**. Its creative is an asset group, built
over three mutates — assets, then the group, then `assetGroupAssets` links carrying an
`AssetFieldType` — in that order, because a group created before its assets is a visible
empty container in the UI while loose assets are only account-level litter. The
assets:mutate response is positional, so the link step sends back the resource name
Google returned rather than one rebuilt from the parsed id, and a wrong-account asset
name is UNCONFIRMED with the ids parsed before it still returned.

The shell differs from Search's in four ways that each matter: `urlExpansionOptOut: true`
so it serves the final URL it was given; no `networkSettings`, which the channel does not
take; `maximizeConversions` by default, with manual CPC and maximize-clicks both refused;
and campaign-level geo, as on Search — which is why proximity stays allowed here while
every other Search-only field is refused. Asset groups are created PENDING, Performance
Max's own spelling of "nothing serves until a human enables it".

Both marketing image shapes and a square logo are required rather than reciprocal, and
headlines, long headlines and descriptions are three distinct asset field types rather
than one list bucketed by length. All of that validates in the pure preflight; the image
bytes are fetched through the same hardened credential-free transport Demand Gen uses,
still before the first budget mutate.

An empty creative stays a supported input: the campaign is created with no asset group
and the closing step says `NO ASSET GROUP` loudly, because such a campaign cannot serve
and a quiet success would not say so.

Two refuse-don't-drop gaps closed with it. `Keywords` and `AudienceSegments` were the last
two fields that validated on every channel and were then attached only on the Search
cascade — Demand Gen's ad group takes no criteria at all and Performance Max has no ad
group to hang them on — so both are now refused with `kind != campaignKindSearch`, the
fence style that auto-refuses the next channel added rather than silently admitting it.
`docs/api-catalog.md` was corrected with them: the `negativeKeywords` entry had contrasted
itself with `keywords`, "which IS ignored there", and that sentence became false the moment
the fence landed.

The four places that duplicate the channel list all grew together: the dispatch create
switch, `googleAdsChannelIsSupported`, `model.AdoptableVariants`, and
`googleAdsVariantForChannelType` / `googleAdsRecordedChannelType`. Monitoring's GAQL
widened to `IN ('SEARCH', 'DEMAND_GEN', 'PERFORMANCE_MAX')`; it will need widening again
for Video and Display.
