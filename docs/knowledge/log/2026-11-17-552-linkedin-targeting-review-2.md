# 2026-11-17 — #552 LinkedIn targeting: review 2 (exclude key list, orgURN test, catalog order, preflight godoc)

**Fix** — David Deal's second #557 review on `linkedInConfig.targeting`, as a follow-up branch so
the approved #557 merges unchanged.

- **Unknown `exclude` key lists only excludable facets.** `cleanFacet` answered an unknown key with
  every key from `TargetingFacetKeys()`, so under `exclude` it offered `groups` and
  `interfaceLocales`, which the next check refuses as include-only. The new `excludableFacetKeys()`
  applies the same `includeOnly` rule as that check, and the exclude refusal lists only its keys.
  Pinned by `TestValidateTargeting_UnknownExcludeKeyListsOnlyExcludableFacets`; without the fix
  the message names both include-only facets.
- **orgURN check on the targeting path is tested.** With `in.Targeting` set, `CreateCampaign`
  skips `validatePrerequisites` and checks the org with `c.orgURN(accountID)` alone.
  `TestCreateCampaign_TargetingRefusesMalformedOrgBeforeAnyRequest` gives the account a
  non-numeric `orgId` (not a `DefaultOrgID` change, which would hit the default-org conflict
  refusal instead) and asserts a pre-flight refusal with zero LinkedIn requests. Without the
  check the client sends the campaign-group search first.
- **Catalog order.** In `docs/api-catalog.md` the `global_marketing_ops` role-gate paragraph moved
  above `##### Probe before enabling`, right after the refusal list, so the probe subsection comes
  last.
- **PreflightCreate godoc.** It said only bidding is a synchronous 400. It now lists what is
  refused there: bidding (`checkBiddingShape`, `linkedinBidPlan`), conversions
  (`checkConversionsShape`, `linkedinConversions`: shape, deployment gate, ids) and targeting
  (`checkTargetingShape`, including a wrong-typed `employerExclusions` when targeting is set, and
  `linkedinTargeting`: deployment gate, `ValidateTargeting`, which also holds `employerExclusions`
  to the employers rule and checks the merged employer cap).
