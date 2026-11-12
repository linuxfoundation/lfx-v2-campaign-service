# 2026-11-13 — #576 A failed create names the upstream campaign it left behind

**Fix** — When a create failed after the upstream campaign existed (for Google Ads,
`campaign_created_setup_incomplete`), the job result named only `campaign_row_id`, although
the row held `platform_campaign_id`. An operator had to search the ad account to find the
half-built paused campaign.

- `dispatchPlatform`'s retained-partial path now sets `campaign_id` from the partial's
  `PlatformCampaignID`, both when the partial is recorded on its row and when recording it
  fails. An id-less partial has no id, and `omitempty` leaves the field out.
- The `campaign_id` description on the job result (`design/brief.go`, regenerated) now says
  it is present whenever the upstream campaign is known to exist.
- The change is in the orchestrator, so it applies to every platform whose dispatcher returns
  a partial with an id, not only Google Ads.
- Test: `TestOrchestrator_ARetainedPartialNamesItsUpstreamCampaign`.
