# 2026-11-06 — #552 LinkedIn per-campaign targeting and matched audiences

**Update** — `linkedInConfig.targeting` (`CampaignInput.Targeting`) sends the caller's own
LinkedIn `targetingCriteria`, the first two gaps of the LinkedIn Ads gap review (#552).

- Each `include` entry (1–10) is one ANDed clause whose facets are ORed, after the geoTargets
  clause; `exclude` is ORed. Facets: skills, groups, industries, titles, seniorities,
  jobFunctions, employers, staffCountRanges, interfaceLocales, and the ad-segment facets
  audienceMatchingSegments (contact and company lists) and dynamicSegments (website and
  engagement retargeting).
- Set, it replaces the targeting profile and the hard-coded job functions and seniority
  exclusions; the config's `employerExclusions` still join `exclude.employers`. Omitted, nothing
  changes.
- `linkedin.ValidateTargeting` refuses, before any job or request, unknown facets, malformed
  values, empty clauses, LinkedIn's per-facet caps, include-only facets under exclude, a value
  both included and excluded, an included employer the config's `employerExclusions` excludes,
  more than 200 employers once those exclusions are merged, and the facet combinations LinkedIn documents as invalid (titles
  with seniorities or job functions, employers with industries or company sizes).
  `PreflightCreate` answers these with a synchronous 400.
- Behind `LINKEDIN_ADS_TARGETING_ENABLED`, off by default, because no shape has been exercised
  against a live account. The ad-segment facets also require `global_marketing_ops`
  (`service.LinkedInMarketingOpsInputs`, checked with the Google inputs in one call).

Built from the LinkedIn Marketing API docs (li-lms-2026-10). Not yet probed live.
