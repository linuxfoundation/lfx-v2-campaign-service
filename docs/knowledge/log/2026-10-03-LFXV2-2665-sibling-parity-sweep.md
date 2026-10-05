# 2026-10-03 — Sibling-parity sweep of the LFXV2-2665 targeting paths

**Verification** — The pre-PR sibling-parity sweep over the new geo, criteria,
extension and ad-group paths, covering tests as well as code. One gap found and
closed; everything else already at parity.

**Code side — no divergence.** `ValidateCampaignInput` exists on the googleads
client alone, so there is no sibling to drift from, and `AdoptExisting` is read
by no dispatcher but `internal/dispatch/googleads.go` — the validate-before-the-
channel-is-resolved defect fixed earlier today therefore has no cross-platform
counterpart to apply. Microsoft's `validateGeoTargets`/`resolveGeoTargets`
(`internal/platform/microsoft/geo.go`) and `validateCpcBid`
(`internal/platform/microsoft/targeting.go`) were read in full: empty-check,
cap, membership, dedupe and a fail-closed restatement in the resolver, matched
one for one by the googleads equivalents. Numeric geo-target-constant ids stay
shape-checked rather than looked up, which is deliberate — a lookup would make
`ValidateCampaignInput` send a request, which its contract forbids.

**Test side — one gap, now closed.** Microsoft has
`TestMicrosoft_UnresolvableGeoTargetCreatesNothing`: an unusable targeting VALUE
on a channel that supports the field must be refused with nothing created. The
googleads side had that shape only for fields predating this work
(`TestGoogleAds_UnmappedGeoFailsDispatchWithNoCampaign`,
`TestGoogleAds_BadServingReadinessConfigIsPreCreate`). Added
`TestGoogleAds_BadTargetingConfigIsPreCreate` — thirteen cases on the SEARCH
channel, one per new validator, each asserting the dispatch is refused, no
campaign is returned, and the budget mutate never ran. It is deliberately the
mirror of `TestGoogleAds_SearchOnlyFieldsAreRefusedOnDemandGenBeforeAnyCreate`:
that table refuses on the CHANNEL, this one on the VALUE.

The remaining sibling families were already matched and needed nothing: the
claim lifecycle (`PreCreateErrorsReleaseClaim`, `AmbiguousCreateRetainsClaim`),
the credential guards (`TrimsWhitespaceCustomerID`,
`ValidateGoogleAdsCredentials_WhitespaceOnlyIsIncomplete`), and — at the
platform layer — mid-cascade partial results, short-mutate-response UNCONFIRMED
handling and pre-mutate refusal for every new family
(`TestCreateCampaign_TargetingCriteriaFailureKeepsCampaignPartial`,
`TestCreateCampaign_LinkFailureStillReportsTheCreatedAssets`,
`TestCreateCampaign_MidCascadeFailureReportsHowFarItGot` and their neighbours).

No behaviour changed; this entry records the gate, not a fix.
