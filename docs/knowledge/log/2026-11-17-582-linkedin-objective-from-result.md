# 2026-11-17 — #582 LinkedIn objective readback reads the result blob; ambiguous reuse aborts

**Fix** — two findings from the PR #637 review.

- The settings readback's recorded `objective_type` for an automatic row came from the config
  snapshot, trusted only when it carried the `lifetimeBudget` key. That key is caller-writable:
  `UpdateCampaign` stores caller JSON verbatim, so `{"lifetimeBudget":false}` passed the check and a
  caller-supplied `objective` could fabricate a match or a divergence. The create now records
  `objectiveType` and `optimizationTarget` in `linkedin.CampaignResult` beside `BidApplied`, and
  `linkedinResultObjective` reads them from the row's Result, which only the service writes. A
  blob without the key predates #582 and reads `WEBSITE_VISIT`, the only automatic objective then.
  `linkedinSnapshotObjective` and `linkedinSnapshotAuthorshipKey` are gone, which supersedes the
  2026-11-13 authorship entry.
- `findCampaignByNameInGroup` reused a same-name `WEBSITE_CONVERSION` campaign that reported no
  `optimizationTargetType` for either bidding mode, so an automatic create could attach to a manual
  campaign. It now aborts. `WEBSITE_VISIT` stays lenient, since only `MAX_CLICK` is created there.
