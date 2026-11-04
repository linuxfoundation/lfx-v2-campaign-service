# 2026-11-04 — #461 reconciled with #469, #472, #481 and #482 on main

**Update** — #470 was rebased onto main, which now carries #469 (Demand Gen audience and
languages), #472 (Display audiences and placement exclusions), #478, #479 (per-channel
`utm_medium`), #481 (Demand Gen video ads and several ad groups) and #482 (Demand Gen
sitelinks, several ads, and every valid asset id kept on an unusable result).

Extensions: #482 admits sitelinks on Demand Gen and #470 admits sitelinks, callouts and
structured snippets on Performance Max. `validateAssetPlan` keeps both: Demand Gen refuses
every type but sitelinks, Performance Max refuses call extensions, promotions, prices and
lead forms, and every other channel but Search refuses the whole set. Each channel's
destinations carry its own `utm_medium` from `utmMediumFor(kind)`: `display` on Demand Gen,
`cpc` on Performance Max.

Audience signal: #459's rule for `DemandGenAudience` now applies to
`PerformanceMaxAudienceSignal` too. `resolvePerformanceMaxAudience` matches the resource
name with `demandGenAudienceRE`, requires a `canonicalCampaignID` id in either spelling, so
`0`, `007` and an id past int64 are refused, and refuses a value that is only whitespace
instead of reading it as absent. The signal payload reuses #469's `audienceInfo`, and the
signal tests use #469's `hasExactStep`; both had been declared again on this branch.

Asset ids: #482's `uniqueIDs` fix covers loops over account-level ASSET results. The
signals loop records `AssetGroupSignal` criterion ids, children of the asset group, under
the three-way ambiguity `GeoCriterionIDs` documents, so it joins the criteria loops #482
left alone on purpose; the googleads concept now lists it there and lists Performance Max
under `createCampaignAssets`' all-or-nothing contract.

Fences: #472's Display-only fields and #481's Demand Gen ad-group fields stay refused on
Performance Max, and the signals stay refused on every channel but Performance Max.
