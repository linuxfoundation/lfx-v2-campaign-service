# 2026-11-13 — #578 Display sends a target CPA as the standalone targetCpa

**Fix** — A Google Display campaign created with a target CPA failed after its budget was
created: Google refused `maximizeConversions.targetCpaMicros` on DISPLAY with
`OPERATION_NOT_PERMITTED_FOR_CONTEXT` (seen in prod on 2026-11-11, campaign-service 1.0.35).

- `validateBiddingPlan` now sets `biddingPlan.standaloneTargetCPA` on Display as well as
  Demand Gen, so a target CPA renders as `targetCpa: {targetCpaMicros}`.
- Search and Performance Max keep the folded `maximizeConversions: {targetCpaMicros}`: the
  same validateOnly probe (2026-11-11, v23) found they refuse the standalone form.
- `displayBiddingStrategies` is now recorded as live-verified for its five automated
  strategies. Manual CPC stays refused on Display for the client-side reason already recorded.
- Tests: `TestBiddingPlan_TargetCPAFormIsPerChannel` and
  `TestCreateDisplayCampaign_TargetCPASendsTheStandaloneStrategy`.
