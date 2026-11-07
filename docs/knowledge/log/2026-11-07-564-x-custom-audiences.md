# 2026-11-07 — #564 X custom audiences at create, and the picker read

**Update** — `twitterConfig.customAudienceIds` and `twitterConfig.excludedCustomAudienceIds`
(gap 3 of the X Ads gap review, #564) target and exclude X custom audiences on the line item an
X create makes, as `CUSTOM_AUDIENCE` criteria with `operator_type` `EQ` and `NE`. A new read,
`GET /projects/{project_id}/twitter-ads/custom-audiences` (`list-twitter-ads-custom-audiences`),
lists the ad account's audiences for a picker.

- The rules live once in `twitter.ValidateCustomAudienceIDs`: each id is 1-32 letters or digits,
  sent exactly as given, at most 100 per list, with no repeat and no id in both lists.
  `TwitterDispatcher.PreflightCreate` refuses a breach, or a value that is not an array of
  strings, with a synchronous 400 (`invalid campaign config: twitterConfig.<field>: ...`).
- Before anything is created, every id is read with
  `GET accounts/:id/custom_audiences?custom_audience_ids=…` and must be live and targetable. A
  miss, a deleted or non-targetable audience, or an unreadable answer creates nothing.
- The exclusions are POSTed first of all the criteria and the inclusions last, after the line
  item and before the promoted tweet, through the #567 criterion path. An `NE` echo without
  `operator_type` is UNCONFIRMED.
- The audiences belong to the shared LF X ad account. So a twitter-ads create that sets either
  list needs `global_marketing_ops` on the project (`service.TwitterMarketingOpsInputs`, the same
  single check as Google, Meta, Reddit and LinkedIn; 403, or 503 when the check cannot be made).
  The read is in the gateway's `marketing-ops-api` rule. The chart route, the RuleSet and
  `parity_test.go` (`TestTwitterCustomAudiencesIsAMarketingOpsRead`) were updated together.
- Default-off deployment flag `TWITTER_CUSTOM_AUDIENCES_ENABLED`; the chart sets `"false"`.
  While off, a create setting either list is a 400 naming the flag and the read answers 400.

Not verified live: no X call was made. The shapes are from X Ads API v12's published
reference. Turn the flag on only after a live create confirms the custom-audience lookup and
the `NE` criterion echo.
