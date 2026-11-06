# 2026-11-06 — Google Ads geo target cap rises from 60 to 500

**Fix** — `maxGeoTargets` (`internal/platform/googleads/geo.go`) bounded `geoTargets` and
`excludedGeoTargets` at 60 each, so LFX refused the live KubeCon NA 2026 Demand Gen YouTube
campaign's 63 locations (50 US state-level ids and 13 Canadian provinces and territories, read-only
GAQL) before any request, and a DMA-level US campaign (about 210 DMAs) as well (#515). The cap is
now 500 per list, still a sanity bound on this service's input rather than a Google limit. Each
list still goes in ONE atomic criteria mutate: validateOnly probes on 2026-11-06 accepted 500
inclusions, 500 exclusions and 20 radius targets of real geo target constants in one
`campaignCriteria:mutate` on a paused LFX Search campaign, and 500 + 500 in one
`adGroupCriteria:mutate` on a paused LFX Demand Gen ad group. The request shape is unchanged. See
`code/internal-platform-googleads.md` (the `maxGeoTargets` paragraph).
