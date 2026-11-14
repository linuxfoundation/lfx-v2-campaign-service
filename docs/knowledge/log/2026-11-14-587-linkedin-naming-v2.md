# 2026-11-14 — #587 LinkedIn campaign naming v2 behind a flag

**Update** — New LinkedIn campaigns can follow LF's own ad set naming convention,
`Events | <event> | <region> | <objective> | <Intent|Prospecting|Retargeting> | <Static|Motion|Social|Text> | <business unit> | <funnel>`,
behind `LINKEDIN_ADS_NAMING_V2_ENABLED` (default off, exactly `"true"` enables it; chart value
`"false"`). Decided by the owner on 2026-11-14 (option O2 of the #587 decision memo).

- Self-serve sends optional `linkedInConfig.naming {region, audienceIntent, businessUnit}`. Each is
  a closed list, matched case-insensitively and stored canonically; a wrong shape or a value off a
  list is a synchronous 400 (`ErrCreateConfigInvalid` pre-flight refusal) whatever the flag says,
  and while the flag is on `businessUnit` is required.
- Region list `NA | EMEA | APAC | LATAM | India | Global` (Meta's and Reddit's buckets); default:
  the one bucket every geo target falls in, else `Global`.
- Audience intent `Intent | Prospecting | Retargeting`; default `Retargeting` for the `retargeting`
  tactic or an included `dynamicSegments` facet, else `Prospecting`. The tactic and slot `#<n>`
  tokens are still appended inside segment 5, so name uniqueness — the key LinkedIn reuses a
  campaign by — is exactly what it was.
- Format derived server-side: `Static` for every campaign today. `campaignNameFormat` is the hook
  where the separate text-ad and video branches map to `Text` and `Motion`.
- Business unit list = lf-dbt's canonical project names: the outputs of `normalize_project_name`
  plus the umbrella names `map_paid_ads_project_id` maps to the LF project id (lf-dbt
  `macros/parse_campaign_name.sql`, origin/main `0b6b6c15d`, unchanged since `2ac0d65dc`). It is a
  copy; a test pins that each value normalizes to itself under a copy of the macro.
- Unchanged: the campaign GROUP name, the objective (bidding) and funnel (row label) segments, and
  every name while the flag is off. Existing campaigns are never renamed.
- Retry safety: the frozen recorded name (previous entry) wins over v2, and when nothing is frozen
  a create in a reused group first looks for a campaign under the LEGACY name and reuses it under
  that name, so turning the flag on can never duplicate a paid campaign.

Not done here, flagged: the decision memo suggests that a `businessUnit` other than the project's
own be a `global_marketing_ops` input; there is no project→BU mapping to compare against yet, so it
is not gated (an executive director can pick any BU in the list).

## Migration note for the data pipeline (lf-dbt / paid reporting)

1. Ship the lf-dbt change BEFORE the flag is turned on: strip a trailing ` <tactic>` / ` #<n>` from
   positional segment 5 (`Retargeting retargeting #2` → `Retargeting`), and confirm every value of
   the business-unit list resolves in `normalize_project_name` / `map_paid_ads_project_id` (it does
   at `0b6b6c15d`; keep the two lists in step).
2. Region `LATAM` and `India` are new to `parse_campaign_region`'s keyword fallback, but a
   well-formed 8-segment name is read positionally, so they are reported as written.
3. `utm_term` changes for creatives created under v2 only (it is derived from the campaign name);
   check that no seed keys `utm_term` on the old `…_linkedin_…_<slug>_…` shape
   (`_silver_fact_ad_session_attribution` uses it as a tie-breaker).
4. Before the flip, LFX LinkedIn rows report region `LinkedIn` and the project slug as BU; after
   it, new campaigns report real values. Old campaigns keep their names (and so their buckets)
   unless Ops renames them by hand in Campaign Manager.
5. Rollout order: lf-dbt (1) → this service released with the flag off → self-serve sends `naming`
   → flip `LINKEDIN_ADS_NAMING_V2_ENABLED` in lfx-v2-argocd (owner approval).

## Still open for the data owners

1. Should Meta, Reddit and X follow the same business-unit rule (their segment 7 is still the slug)?
2. Confirm the business-unit list and each foundation's BU — is an event's BU the event org
   (`LF Events`) or the foundation (`Cloud Native Computing Foundation (CNCF)`)?
3. Is the region list `NA | EMEA | APAC | LATAM | India | Global` right?
4. May LFX keep ` <tactic>` / ` #<n>` inside segment 5 with dbt stripping them, or where else should
   a disambiguator go without breaking the 8-segment parse?
5. Does a manual rename of an existing campaign re-bucket its history in
   `linkedin_ads__campaign_report`, or should old names be left alone?
6. Is the campaign group name used anywhere in reporting (LFX keeps its slug-based group name)?

Tests (each proven to fail with its part of the change removed): `TestBuildCampaignNameV2`,
`TestValidateNaming`, `TestNameBusinessUnits_AreFixedPointsOfTheDbtNormalizer`,
`TestCreateCampaign_NamingV2NamesANewCampaign`,
`TestCreateCampaign_NamingV2ReusesALegacyNamedCampaignInTheGroup`,
`TestCreateCampaign_NamingV2CreatesInAReusedGroupWithoutALegacyCampaign`,
`TestCreateCampaign_FrozenNameWinsOverNamingV2`, `TestCreateCampaign_NamingV2RefusedBeforeAnyRequest`,
`TestLinkedIn_NamingPreflight`, `TestLinkedInDispatch_NamingGateSelectsTheName`,
`TestLinkedInDispatch_NamingV2NeverDuplicatesALegacyCampaign`.
