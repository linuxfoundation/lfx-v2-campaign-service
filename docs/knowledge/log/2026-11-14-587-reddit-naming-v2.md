# 2026-11-14 — #587 Reddit campaign naming v2 behind a flag

**Update** — Following the owner's 2026-11-14 decision to use the business-unit naming on every
platform, new Reddit campaigns can follow LF's convention,
`Events | <event> | <region> | <objective> | <audienceIntent>[ <tactic>][ #<n>] | <format> | <business unit> | <funnel>`,
behind `REDDIT_NAMING_V2_ENABLED` (default off, exactly `"true"` enables it; chart value `"false"`).

- Request: optional `redditConfig.naming {region, audienceIntent, businessUnit}` (the shared
  `campaignname.Naming`), validated in `PreflightCreate` (synchronous 400, `ErrCreateConfigInvalid`
  pre-flight refusal) and again in `Dispatch` before any request, whatever the flag says;
  `businessUnit` is required while it is on. For an event it is the owning foundation's business
  unit (`LF Events` only for an LF-run event with no foundation).
- Region default: the one bucket every targeted country falls in (`geoTargets`, or the US default
  when empty; `campaignname.CountryRegions`), else `Global`. The legacy name reports the first
  country's bucket, so a US + UK campaign moves from `NA` to `Global`.
- Intent default: `Retargeting` for the `retargeting` tactic, else `Intent` (the legacy value).
- Format: derived — `Motion` for the `video_views` objective, else `Social` when `postUrl` promotes
  an existing post (its content is not read; `campaignname.FormatSocial` now documents that
  meaning), else `Static` (the image post the client authors). The legacy name said `Social` for
  every campaign. `campaignNameFormat`
  is the hook for other post kinds (carousel → `Carousel`).
- Unchanged: objective and funnel (`ToFU` default) segments, the tactic and slot tokens inside
  segment 5, the ad group name, and every name while the flag is off.
- Retry safety: Reddit never reuses a campaign or ad group by name (`BidApplied` is always true on a
  successful create), so there is no legacy-name lookup to add; the guard is the recorded name — the
  dispatcher passes `model.DispatchCampaignName` as `FrozenCampaignName`, used verbatim over both
  names, so a re-dispatch of a slot keeps the name its partial recorded.

## Data pipeline (lf-dbt) — Reddit

Reddit names go through `reddit_ads__campaign_report` → `_silver_fact_campaign_report`
(`ad_platform = 'reddit_ads'`, de-duplicated against `reddit_ads_education`) →
`silver_fact_paid_ads`, the same positional 8-segment parser as LinkedIn and Meta (lf-dbt origin/main
`0b6b6c15d`, read-only). Required before the flag is turned on:

1. The shared segment-5 strip (` <tactic>` / ` #<n>`), one change for every platform.
2. `ad_format` becomes `Static` or `Motion` for v2 Reddit campaigns that do not promote an existing
   post (those stay `Social`).
3. `project` becomes a canonical business unit. Note the education connector: `silver_fact_paid_ads`
   falls back to `LF Education` for `reddit_ads_education` rows only when the positional segment 7
   is NULL, so a v2 campaign on the education account reports whatever business unit the caller
   sent — self-serve must send `LF Education` there. No dbt change, but worth a test in lf-dbt.
4. `region` may change for multi-country campaigns (first country → `Global`).
5. `utm_term` is unaffected (Reddit's is derived from the event name).
6. Rollout: lf-dbt (1) → this service released with the flag off → self-serve sends `naming` →
   flip `REDDIT_NAMING_V2_ENABLED` in lfx-v2-argocd (owner approval).

Tests (each proven to fail with its part of the change removed): `TestBuildRedditCampaignNameV2`,
`TestCreateCampaign_RedditNamingV2NamesANewCampaign`, `TestCreateCampaign_RedditFrozenNameWinsOverNamingV2`,
`TestCreateCampaign_RedditNamingV2RefusedBeforeAnyRequest`, `TestReddit_NamingPreflight`,
`TestRedditDispatch_NamingRefusedBeforeAnyRequest`, `TestRedditDispatch_NamingGateSelectsTheName`,
`TestRedditDispatch_FrozenNameWinsOverNamingV2`.
