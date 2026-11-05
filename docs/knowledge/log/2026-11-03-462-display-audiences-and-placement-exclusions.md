# 2026-11-03 — #462 Display audiences and placement exclusions

**Update** — A Display campaign can now be targeted at audiences and kept off sites.
`googleAdsConfig.audienceSegments`, refused on Display until now, is accepted there, and
three Display-only segment lists join it — `customAudiences`, `customAffinities`
(`customInterests` resources) and `userInterests` — each a list of EXISTING resource names
on the campaign's own account; this service creates no audience. `excludedPlacements` takes
bare domains and attaches them as campaign-level negative placement criteria. All five are
refused on every other channel.

Every name is checked for shape and for THIS account before the budget mutate, so a
malformed or foreign-account name never strands a campaign; a name of the wrong kind is
refused with the field it belongs in, and an Audience resource with Display's reason — the
channel is not audience-grouped and Google refuses an `audience` criterion there. Placements
are refused rather than rewritten when they carry a scheme, path, query or port, or are an
IPv4 literal; the only change is lower-casing.

On Display the audiences are attached as TARGETING criteria, where Search's are
observation-only: when any audience list is given, the ad group is created with
`targetingSetting` AUDIENCE `bidOnly: false` (after Google's four demographic defaults,
restated unchanged), accepted by a `validateOnly` probe; without one its payload is
unchanged. Optimized targeting is left at Google's default and may widen
reach. The account check is account-scoped, not tenancy protection — an accepted risk
tracked by #468. The segments go in one atomic `adGroupCriteria:mutate` after the ad group, the
placements in one atomic `campaignCriteria:mutate` after that, each with the partial-result
and UNCONFIRMED contract, its ids on the result (`audienceCriteriaIds`,
`excludedPlacementCriteriaIds`) and a step line. The closing step's no-geo remedy now says
"add geo targeting".

What Google accepts was established by `validateOnly` probes on 2026-11-03 (v23). Multiple
Display ad groups are a follow-up. See
[internal/platform/googleads](../code/internal-platform-googleads.md) ("Display campaign
creation") and [internal/dispatch](../code/internal-dispatch.md).
