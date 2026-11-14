# 2026-11-14 — #587 X campaign naming v2 behind a flag

**Update** — Following the owner's 2026-11-14 decision to use the business-unit naming on every
platform, new X campaigns can follow LF's convention,
`Events | <event> | <region> | Traffic | <audienceIntent>[ <tactic>][ #<n>] | <format> | <business unit> | <funnel>`,
behind `TWITTER_NAMING_V2_ENABLED` (default off, exactly `"true"` enables it; chart value `"false"`).

- Request: optional `twitterConfig.naming {region, audienceIntent, businessUnit}` (the shared
  `campaignname.Naming`), validated in `PreflightCreate` (synchronous 400, `ErrCreateConfigInvalid`
  pre-flight refusal) and again in `Dispatch` before any request, whatever the flag says;
  `businessUnit` is required while it is on. For an event it is the owning foundation's business
  unit (`LF Events` only for an LF-run event with no foundation). `naming` is classified as ungated
  for the global_marketing_ops check, as for Reddit.
- Region default: the one bucket every `twitterConfig.geoTargets` country falls in
  (`campaignname.CountryRegions`), else `Global`. With no geo targets (the line item serves
  worldwide, and `geoTargets` is itself behind `TWITTER_CREATE_TARGETING_ENABLED`) it stays `Global`,
  the legacy name's fixed value.
- Intent default: `Retargeting` for the `retargeting` tactic, else `Prospecting` (the legacy value).
- Format: derived from the promoted post — `Social` when `tweetId` promotes an existing post (its
  content is not read; `campaignname.FormatSocial` now documents that meaning), else `Static` for
  an authored post with an image and `Text` without. The legacy name said `Promoted Post`.
  `campaignNameFormat` is the hook for video (`Motion`) and carousel (`Carousel`) posts.
- Objective: `Traffic` (owner decision, 2026-11-14), matching the line item's `WEBSITE_CLICKS`
  objective and the word the LinkedIn and Meta names use for a click campaign. The legacy name keeps
  `Awareness`.
- Unchanged: the funnel, the tactic and slot tokens inside
  segment 5, the line item name, and every name while the flag is off.
- Retry safety: X reuses a campaign by name account-wide (`findCampaignByName`), exactly the
  LinkedIn situation. The recorded name (`model.DispatchCampaignName` → `FrozenCampaignName`) is
  used verbatim; when nothing is frozen and the v2 name finds nothing, the LEGACY name is looked up
  too, and a campaign found there is reused under its legacy name (one extra GET per create while
  the flag is on). A failed lookup returns before anything billable is created.

Resolved (owner, 2026-11-14): the line item is created with objective `WEBSITE_CLICKS` while the
legacy name's objective segment has always said `Awareness`; under v2 X reports as `Traffic`.

## Data pipeline (lf-dbt) — X

X names go through `twitter_ads__campaign_report` → `_silver_fact_campaign_report`
(`ad_platform = 'twitter_ads'`) → `silver_fact_paid_ads`, the same positional 8-segment parser
(lf-dbt origin/main `0b6b6c15d`, read-only). Required before the flag is turned on:

1. The shared segment-5 strip (` <tactic>` / ` #<n>`), one change for every platform.
2. `region` stops being a constant `Global` for LFX X campaigns with geo targets (`NA`, `EMEA`, …).
3. `ad_format` becomes `Social`, `Static` or `Text` instead of `Promoted Post`; none of the three is
   matched by `parse_campaign_ad_format` as `Promoted Post` was not either, so the positional read
   is what reports them — dashboards grouping X by format should expect the new values.
4. `objective` moves from `Awareness` to `Traffic` for v2 X campaigns (positional segment 4), so X
   click campaigns join LinkedIn's and Meta's `Traffic` bucket; legacy X campaigns keep reporting
   `Awareness`. Any X objective dashboard or filter should expect both during the transition.
5. `project` becomes a canonical business unit (see the LinkedIn entry for keeping the lists in
   step).
6. `utm_term` is unaffected (X's is derived from the event name).
7. Rollout: lf-dbt (1) → this service released with the flag off → self-serve sends `naming` →
   flip `TWITTER_NAMING_V2_ENABLED` in lfx-v2-argocd (owner approval).

Tests (each proven to fail with its part of the change removed): `TestBuildTwitterCampaignNameV2`,
`TestCreateCampaign_TwitterNamingV2NamesANewCampaign`,
`TestCreateCampaign_TwitterNamingV2ReusesALegacyNamedCampaign`,
`TestCreateCampaign_TwitterFrozenNameWinsOverNamingV2`,
`TestCreateCampaign_TwitterNamingV2RefusedBeforeAnyRequest`, `TestTwitter_NamingPreflight`,
`TestTwitterDispatch_NamingRefusedBeforeAnyRequest`, `TestTwitterDispatch_NamingGateSelectsTheName`,
`TestTwitterDispatch_NamingV2NeverDuplicatesALegacyCampaign`,
`TestTwitterDispatch_FrozenV2NameSurvivesTheGateTurningOff`. The `Traffic` objective was proven the
same way: with the test names updated and `nameObjective` still `Awareness`,
`TestBuildTwitterCampaignNameV2`, `TestCreateCampaign_TwitterNamingV2NamesANewCampaign` and
`TestTwitterDispatch_NamingGateSelectsTheName` fail.
