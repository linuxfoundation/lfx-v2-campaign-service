# 2026-11-16 — #563 Microsoft settle window for absent ad groups, temporary bid refusal, display URL at preflight

**Fix** — The ad-group findings of two #613 review rounds, which landed in mixed entries on #613
and are collected here for the PR that carries the ad group code.

- The bid write's reconciliation trusts a definitely-absent unconfirmed ad group only after
  `microsoftUnconfirmedSettleWindow` (1 hour), measured from the campaign row's `UpdatedAt`. That
  is the last row write, never earlier than the dispatch result that recorded the group;
  `CreatedAt` is the claim, before the create ran. A timed-out create can still commit, so inside
  the window, or when `UpdatedAt` is unknown, absence keeps the refusal. A group found with a usable
  id still recovers at once. `MicrosoftDispatcher.bidNow` is the clock seam.
- Inside the window the refusal is `settlingBidWriteError` (`internal/dispatch/bid.go`, wrapping
  `ErrBidUnwritable`). Its `BidSettlingReason` is a service-authored sentence, retry time first
  (`UpdatedAt` + window, RFC 3339), then the entry. `UpdateCampaignBid` maps it, before the generic
  `ErrBidUnwritable` text, to a 409 that says the refusal is temporary; the reason goes through
  `safeErrSummary`, which clips it at 200 runes. A row with no `UpdatedAt` never settles, so it
  keeps the generic 409.
- Each requested ad group's display URL (the brief's registration domain plus its path1/path2) is
  checked at the create preflight. `ValidateAdGroupDisplayURLs` is called from the new optional
  `service.BriefCreatePreflighter` (`MicrosoftDispatcher.PreflightCreateBrief`, run by
  `Orchestrator.PreflightCreateBrief` with the loaded brief, fail-open except for
  `ErrCreateConfigInvalid`), so an overflow is a synchronous 400.
