# 2026-11-06 — #549 Reddit custom audiences (retargeting and suppression)

**Creation** — Gap 1 of the Reddit Ads gap review (#549). A Reddit ad group can now target and
exclude custom audiences, and a picker can list them.

- `redditConfig.customAudienceIds` / `excludedCustomAudienceIds` are sent as the ad group's
  `targeting.custom_audience_ids` / `excluded_custom_audience_ids` on every ad-group attempt.
  The communities/interests fallback retry keeps them.
- Refused synchronously with 400 before any job: a wrong JSON type, an id that is not 1-64
  letters, digits or underscores, a repeat, an id in both lists, more than 100, or the flag off.
  `Dispatch` and the client repeat the checks before any Reddit call.
- A reddit-ads create that sets either list needs `global_marketing_ops`
  (`service.RedditMarketingOpsInputs`, in the same single check as the Google inputs, 403 /
  503). A new test makes every `redditConfig` field choose gated or ungated.
- `GET /projects/{project_id}/reddit-ads/custom-audiences` (`list-reddit-ads-custom-audiences`)
  lists the audiences on the account a create uses: up to 10 pages, `truncated` when more
  remained, sorted by name. It sits in the gateway's `marketing-ops-api` rule.
- Both are off unless `REDDIT_CUSTOM_AUDIENCES_ENABLED` is `"true"` (chart default `"false"`).

Built from Reddit's public v3 OpenAPI document (`components_schema_targeting`,
`GetCustomAudienceResponse`), fetched 2026-11-06. No Reddit call of any kind was made; turn the
flag on only after a live PAUSED create and a live list confirm the shapes.
