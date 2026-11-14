# 2026-11-14 — #552 LinkedIn: the reused-campaign targeting stop is not reusable; configured employer exclusions meet the employers rule

**Fix** — two #557 review findings on `linkedInConfig.targeting`.

- **Stop status.** The stop on a campaign found by name with `targeting` set returned the
  campaign id and no creative, which `campaignFromLinkedIn` mapped to `created_degraded`. The
  orchestrator preserves that status and reuses it as complete, so repeating the request reported
  success although the targeting was never checked or applied. The client now sets
  `CampaignResult.TargetingNotApplied` on that partial and the dispatcher stamps it `pending`, the
  existing retained-partial-orphan state: the campaign id stays on the row for repair, and
  `isReusableCampaign` refuses it, so a repeat answers reconciliation required without calling
  LinkedIn. Microsoft's equivalent stop reaches the same state (its `created` row returned with an
  error is flattened to `pending`). Pinned by `TestLinkedIn_ReusedCampaignTargetingStopIsNotReusable`
  and `TestOrchestrator_TargetingStopIsNotReusedOnRetry`.
- **Configured employer exclusions.** On the targeting path `employerExclusions` went through
  `validFacets` alone, which accepts any digits: `urn:li:company:01111` passed although
  `exclude.employers` refuses it, and escaped the overlap check beside an included
  `urn:li:organization:1111`. `targetingEmployerExclusions` now applies `cleanFacet`'s `employers`
  rule after `validFacets`, in both `ValidateTargeting` and `buildCustomTargetingCriteria`. Such a
  value is refused in the synchronous pre-flight, before anything is created; only creates that
  set `targeting` (new in #552, behind `LINKEDIN_ADS_TARGETING_ENABLED`) are affected, and the
  profile path is unchanged. Pinned by `TestTargetingEmployerExclusions_MeetTheEmployersRule`.
