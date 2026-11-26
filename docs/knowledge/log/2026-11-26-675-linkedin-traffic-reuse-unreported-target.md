# 2026-11-26 — #675 LinkedIn: an automatic traffic create no longer reuses a campaign with an unreported bidding target

**Fix** — PR #676 review (Copilot, `internal/platform/linkedin/client.go`). Under `WEBSITE_VISIT`,
`findCampaignByNameInGroup` aborted on a same-name campaign with no reported
`optimizationTargetType` only for a MANUAL request; an automatic `MAX_CLICK` request still reused it.
Now that #675 can create manual traffic campaigns (which stay manual after
`LINKEDIN_ADS_TRAFFIC_MANUAL_BID_ENABLED` is switched off), that reuse could attach creatives to a
campaign still bidding its manual CPC while the row records automatic bidding.

- Under `WEBSITE_VISIT` an unreported target now aborts in both directions, with the existing
  "cannot be confirmed to bid …; check its bidding in Campaign Manager" error. `costType` and
  `unitCost` are not consulted: manual CPM exists and automatic is CPM too, so neither proves the
  mode. `WEBSITE_CONVERSION` is unchanged (an automatic request aborts, a manual one reuses).
- The assumption is the one PR #637 already made for `MAX_CONVERSION`: LinkedIn reports the target
  on a campaign created with one, so this service's own automatic traffic campaigns are still
  reused on retry.

Tests: `TestFindCampaignByNameInGroup_WebsiteVisitBiddingMode` replaces "automatic reuses
unreported" with "automatic refuses unreported", plus unreported rows reporting a manual
`CPC` + `unitCost` and a `CPM` costType, both refused. Reuse fixtures that stand for this service's
automatic traffic campaigns (`conversions_test.go`, `name_region_test.go`,
`targeting_criteria_test.go`, `videos_test.go`, the objective-mismatch table, and the stateful fake
in `internal/dispatch/linkedin_x_multi_campaign_test.go`, which now echoes the created campaign's
target) report `optimizationTargetType` as LinkedIn does.
