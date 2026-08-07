# 2026-08-05 — LFXV2-2901: a stale If-Match on update-campaign is 412, not 400

**Fix** — Stale `If-Match` on `UpdateCampaign` was misclassified as 400 instead of
412 (Cursor Bugbot finding, PR #78 review).
Item 2 of [`2026-08-05-lfxv2-2901-claim-returning-and-validate-before-claim.md`](2026-08-05-lfxv2-2901-claim-returning-and-validate-before-claim.md)
moved the status-mismatch validation ahead of
`ClaimCampaignVersion` to stop a rejected request from bumping the version. That
introduced a new bug: without checking the `If-Match` version first, the
status-mismatch check validates the client's payload against whatever row is
CURRENTLY in the database, not the version the client actually read. A concurrent
`ToggleCampaignStatus` can flip `existing.Status` between the client's read and this
request, so a stale-but-otherwise-valid update gets compared against the new status
and rejected with 400 ("use the status-toggle endpoint") — the wrong error for what
is actually a stale-ETag conflict.
Fixed by checking `existing.Version != version` immediately after loading `existing`,
before the status-mismatch check, mirroring the pattern already used correctly in
`UpdateAudience` (`internal/service/audience.go`). Added
`TestBriefService_UpdateCampaign_StaleVersionIsPreconditionFailed` to cover a stale
If-Match against a row whose status changed concurrently.
