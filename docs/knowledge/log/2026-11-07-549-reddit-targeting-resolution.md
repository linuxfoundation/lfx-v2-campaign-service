# 2026-11-07 — #549 Reddit community validation and interest resolution before create

**Update** — `redditConfig.subreddits` and `redditConfig.interests` can now be resolved before
anything is created (gaps 4 and 8 of the Reddit gap review, #549). Before this, a bad community
name, or any of the brief's human interest labels, reached the ad group and was dropped by the
400 fallback.

- `reddit.ResolveCommunities` confirms names with `GET /targeting/communities?names=…` and
  returns Reddit's spelling. `reddit.ResolveInterests` maps each entry to an id from
  `GET /targeting/interests`, matching an id or exactly one name and ignoring case.
- `RedditDispatcher.PreflightCreate` refuses an unknown value with a synchronous 400
  (`invalid campaign config: redditConfig.subreddits|interests: entry N …`). It fails open when
  the read cannot run. `Dispatch` repeats the lookups before the campaign is created and fails
  closed. The orchestrator now records a pre-check refusal that came from a platform read
  (`UpstreamConsulted`) as a successful upstream call.
- `NormalizeSubreddits` replaces the inline subreddit normalization in `CreateCampaign`, with no
  behavior change.
- Default-off deployment flag `REDDIT_TARGETING_RESOLUTION_ENABLED`; the chart sets `"false"`.
  Not role-gated.
- Docs: the internal-service-rules KB "those four" wording now matches the #550 Meta conversions
  read (a nit left open on #572).

Not verified live: no Reddit call was made.
