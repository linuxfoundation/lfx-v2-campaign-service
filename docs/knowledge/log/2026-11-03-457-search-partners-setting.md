# 2026-11-03 — #457 Search campaigns can opt into Search partners

**Update** — A Search campaign was always created on Google Search only:
`networkSettings.targetSearchNetwork` was hard-coded false, with no config field. The live
KubeCon NA 2026 Search campaign runs on Search partners too, so an LFX-built one could not
match it.

`googleAdsConfig.searchPartners` (`internal/dispatch/googleads.go`) maps to
`CampaignInput.SearchPartners` (`internal/platform/googleads/campaign.go`), sent as
`targetSearchNetwork` on the Search create. Omitted or false keeps today's payload byte for
byte. It is refused on every other channel in the pure preflight, as every Search-only field
is. The dispatch comment that counted the Search-only fields now names no count.

Tests: `TestGoogleAds_SearchPartnersReachesTheSearchCampaign` (set and absent, through
dispatch) and `TestValidateCampaignInputKind_RefusesSearchPartnersOffSearch`.
