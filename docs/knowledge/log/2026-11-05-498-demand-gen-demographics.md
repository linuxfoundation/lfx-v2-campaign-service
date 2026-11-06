# 2026-11-05 — #498 Demand Gen demographics inside the Audience

**Update** — `googleAdsConfig.demandGenDemographics` (`CampaignInput.DemandGenDemographics`)
gives a Demand Gen campaign demographic targeting: age ranges, genders, income ranges and
parental statuses, in the `excluded*` vocabularies, plus optional existing user lists
(`audienceSegments`). Before the budget, the cascade creates ONE Audience
(`audiences:mutate`) holding them as `AudienceDimension` entries. It attaches that Audience to
the single Demand Gen ad group, or to every `demandGenAdGroups` entry, as the one `audience`
criterion, the way `demandGenAudience` is attached. This is the shape of the live KubeCon NA
2026 Demand Gen Audience ("CNCF - KubeCon NA - High Intent": `age {25-54,
includeUndetermined}` beside its segments, read-only GAQL).

- `UNDETERMINED` becomes the dimension's `includeUndetermined`. A list holding only
  `UNDETERMINED` is refused. Adjacent age brackets merge into one `AgeSegment`, and 65+ has no
  `maxAge`.
- The name is `<campaign name> | Demographics <hash of campaign name and dimensions>`, shortened
  to Google's 255-character limit. On a retry, Google answers `NAME_ALREADY_IN_USE`, and the
  Audience is read by its exact name and reused if it is ENABLED, CUSTOMER-scoped, has no
  exclusions and holds these dimensions in any order. It is never edited. A definite failure of
  this step returns no result. A create that may have landed returns a partial naming the
  planned Audience, and a budget failure after it returns a partial naming the Audience.
- Pre-flight refusals: the field off Demand Gen; an empty object; the field beside
  `demandGenAudience` or any group `audience`; unknown values; and user lists that are not
  `customers/<this account>/userLists/<id>`. The `excluded*` fields stay refused on Demand Gen.
  The off-channel refusal names only demographic fields that channel accepts.
- Gates: `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED` covers the whole field. `audienceSegments`
  also need `GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED`, and `global_marketing_ops` on create, as
  existing account segments do. Demographics without user lists do not need the role.
- New result field: `demandGenDemographicAudience`, plus a `Demographic Audience created|reused`
  step.

Probes run on 2026-11-05 against account 8666746580 at v23, validateOnly, with nothing persisted:
- **Accepted:** all four dimensions; the same with a user list; 65+ with no `maxAge`; two
  non-adjacent age segments; a temporary Audience attached to an ad group of a PAUSED LFX
  Demand Gen campaign; and an existing Audience attached there.
- **Refused:** `UNDETERMINED` as a value (`INVALID_ENUM_VALUE`); a dimension with only
  `includeUndetermined` (`DIMENSION_INVALID`); an age segment of 30-50 (`DIMENSION_INVALID`); a
  256-character name (`TOO_LONG`); and an existing name (`NAME_ALREADY_IN_USE`).

Not verified: a persisted (non-validateOnly) create and its GAQL read-back, which the reuse
comparison relies on. That is why the field stays behind `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED`.
