# 2026-11-17 — #563 Microsoft bid: reconciliation reasons reach the caller

**Fix** — from the Copilot review of PR #623:

- The two bid refusals for an unconfirmed later ad group that cannot be reconciled returned a
  plain `ErrBidUnwritable`, so `UpdateCampaignBid` answered the generic bidding-setup 409 and
  their remedies never reached the caller. They now return `reconcileBidWriteError` (wrapping
  `ErrBidUnwritable`), which carries a service-authored reason (the entry label and the remedy,
  never Microsoft's text) and whether a retry can clear it.
- `UpdateCampaignBid` maps it before the generic text, with the reason clipped by
  `safeErrSummary`: a group with no recorded or recomposable name gets a 409 saying retrying will
  not help — set the bid in Microsoft Advertising or recreate the campaign; a lookup that could
  not prove the group found or absent gets a 409 saying it is usually temporary — retry later.
  Every other `ErrBidUnwritable` keeps the generic 409; the upstream cause goes to the log only.
- Also from the lfx-one review: `CampaignResult.ConversionGoalIDs` was declared here but used only
  by the conversion-goal work in #622, which declares it, so it is removed from this branch.

Not verified live: no Microsoft call was made.
