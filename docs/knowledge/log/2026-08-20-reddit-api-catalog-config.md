# 2026-08-20 — Reddit: document redditConfig in the API catalog

**Docs** — The local-review trio flagged `api-catalog-platform-config-drift`: the
Reddit author-a-post change (commit c71ae2c8) added `imageUrl` and `callToAction`
to `redditConfig`, but `docs/api-catalog.md` carried only a one-line
`redditConfig?: object — Reddit-specific params` stub. Because
`CreateCampaigns.config` is typed `Any` in the Goa design, the catalog is the
effective consumer-facing validation contract, and CreateCampaigns is async (a
bad config surfaces as a failed job, not a synchronous 400), so an undocumented
field gives the caller nothing to debug against.

Added a full `#### RedditConfig (the redditConfig object)` section mirroring the
existing `MetaConfig`/`GoogleAdsConfig`/`MicrosoftConfig` sections: it documents
every field (`budgetUsd`, `startDate`/`endDate`, `objective`, `geoTargets`,
`subreddits`, `interests`, `keywords`, `variants`, `postUrl`, the new `imageUrl`
and `callToAction`, `conversionPixelId`, `videoGoal`) with types, defaults,
validation, and the `postUrl`-wins / snapshot-sanitization rules. The envelope
bullet now reads "(see RedditConfig below)". This closes the drift for the whole
struct, not just the two new fields (it was never documented, even for `postUrl`).
