# 2026-11-06 — #549 Reddit create-time targeting exclusions

**Update** — `redditConfig.excludedSubreddits` and `redditConfig.excludedGeoTargets` (gap 3 of
the Reddit Ads gap review, #549) are sent at create time as the ad group's
`targeting.excluded_communities` and `targeting.excluded_geolocations`. LF event campaigns
need them to keep ads out of communities like r/politics, and to carve countries out of a
regional target.

- Rules live once in `reddit.NormalizeTargetingExclusions`. `r/` is stripped, and a subreddit
  name must be 2–21 letters, digits or underscores. Country codes must be assigned ISO alpha-2
  codes. Duplicates are dropped. An exclusion that is also targeted is refused, and that
  includes the `US` default. The caps are 100 subreddits and 50 countries.
- Synchronous 400 (`invalid campaign config: redditConfig.<field>: ...`) from
  `RedditDispatcher.PreflightCreate` for a rule breach or a value that is not an array of
  strings. A case-variant key is checked too, because encoding/json matches keys
  case-insensitively. The gate and rules run on the best-effort decode, as the bid plan does,
  so an unrelated mistyped field cannot turn a gated or invalid list into an accepted job.
  Dispatch and the client re-check before any Reddit call.
- Both are sent on every ad-group attempt. The communities/interests retry never drops an
  exclusion.
- Default-off deployment flag `REDDIT_TARGETING_EXCLUSIONS_ENABLED`; the chart sets `"false"`.
  It names no account resource and is not role-gated. It keeps a shape no live create has sent
  off the shared ad account.

Not verified live: no Reddit call was made. The shape is from Reddit's published v3 OpenAPI
document (`components_schema_targeting`). Turn the flag on only after a live create confirms
that Reddit accepts both keys, and shows how it answers an excluded community that does not
exist.
