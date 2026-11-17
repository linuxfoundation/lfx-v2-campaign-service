# 2026-11-16 — #552 LinkedIn targeting: review fixes (segment overlap, deterministic refusals, contract text)

**Fix** — the #557 review findings from David Deal and Shiv on `linkedInConfig.targeting`.

- **Segment overlap across facets.** `ValidateTargeting`'s "both included and excluded" check
  compared values only under the same facet key, but `audienceMatchingSegments` and
  `dynamicSegments` both take `urn:li:adSegment:<id>`. One segment included under one and excluded
  under the other passed, and the campaign POST would have included and excluded the same audience
  after the campaign group existed. `segKey` now maps both facets to one namespace when collecting
  the included values and in the lookup, so that is a synchronous 400. Pinned by the cross-facet
  cases in `TestValidateTargeting`.
- **Deterministic refusals.** `ValidateTargeting` ranged over each include clause and `Exclude` as
  Go maps and returned on the first bad facet, so an identical request could get a different 400.
  It now walks them with `sortedKeys`. Pinned by `TestValidateTargeting_RefusalIsDeterministic`.
- **Dispatch's shape check is tested.** `TestLinkedIn_DispatchRefusesMisspelledTargetingKey`
  drives `Dispatch` with the flag on and a misspelled `excldue`: `ErrCreateConfigInvalid`,
  `NoUpstreamCreate()`, no upstream request. Without the `checkTargetingShape` call in `Dispatch`
  the lenient decode let it reach LinkedIn.
- **Dead helper removed.** `(*Targeting).SegmentFacetsSet` had no caller outside its test; it and
  the test are gone, and `service.LinkedInMarketingOpsInputs` names the segment facets through
  `linkedin.FacetAudienceMatchingSegments` and `FacetDynamicSegments` instead of string literals.
- **Contract text.** The Goa `config` description, `docs/api-catalog.md` and the package concept
  now say conversions are associated only on a created campaign, or one reused by name when
  `targeting` is not set; the TARGETING paragraph describes the reused-campaign stop (`pending`,
  `targetingNotApplied`) and lists every synchronous refusal (unknown key or wrong shape,
  wrong-typed or null-holding `employerExclusions`, an included employer in `employerExclusions`,
  more than 200 employers after the merge). The catalog's refusal lead-in covers both message
  prefixes, and its new "Probe before enabling" section carries the probe the
  `LINKEDIN_ADS_TARGETING_ENABLED` comments in `pkg/constants`, the README and the chart values now
  point to.
