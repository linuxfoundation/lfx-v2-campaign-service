# 2026-11-03 — #461 Performance Max search themes, an audience signal and campaign-level extensions

**Update** — A Performance Max campaign was created with an asset group carrying no
signals, and sitelinks, callouts and structured snippets were refused on the channel, so an
LFX-built campaign could not match the reference KubeCon Performance Max campaign, which has
search themes, an audience signal, sitelinks and a structured snippet.

`googleAdsConfig.performanceMaxSearchThemes` and `performanceMaxAudienceSignal`
(`internal/dispatch/googleads.go`) map to `CampaignInput.PerformanceMaxSearchThemes` and
`PerformanceMaxAudienceSignal`. `pmax_signals.go` validates them in the pure preflight —
refused off Performance Max, refused without a creative (no asset group to attach to),
themes trimmed, non-empty, ≤80 runes, case-insensitive duplicates refused, at most 25 (the
limit Google documents, not probed), and one audience as a numeric id or a resource name in
this account — and creates them after the asset group in one atomic `googleAds:mutate`,
recording criterion ids in `AssetGroupSignalIDs` and a step line. `createPerformanceMaxAssetGroup`
now returns the group's resource name so the signals name the group as Google returned it.

`validateAssetPlan` admits Performance Max for sitelinks, callouts and structured snippets,
which the PMax cascade links at the campaign level through Search's `createCampaignAssets`
before the asset group; call extensions, promotions, prices and lead forms stay Search only.
The Performance Max demographic-exclusion refusal no longer says this client creates no
asset group signals.

Live facts come from `validateOnly` probes on 2026-11-03 (v23) only: the theme and
audience signal shapes, the 80-character theme limit, several themes and one audience per
request, one audience per asset group, and `campaignAssets` links of the three field types
on a Performance Max campaign.
