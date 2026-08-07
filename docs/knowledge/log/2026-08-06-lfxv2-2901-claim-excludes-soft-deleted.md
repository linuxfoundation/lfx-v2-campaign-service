# 2026-08-06 — The claim pair excludes soft-deleted campaigns (LFXV2-2901)

**Update** — `ClaimCampaignVersion`'s guarded read and its no-row classifier now
carry `AND status <> 'deleted'`, matching every other `campaigns` read. Cursor and
Copilot flagged this independently, which in this repo has been a reliable signal
that a finding is real.

Soft delete here is a status value, not a `deleted_at` column
(`model.CampaignStatusDeleted`). `getCampaignQuery`, `getCampaignByPlatformQuery`,
and `replaceCampaignQuery` all filter on it, so a soft-deleted campaign reads as
ABSENT everywhere — except, until now, in the claim. That asymmetry is worse than
a stale read: a claim on a deleted row whose `version` happens to match SUCCEEDS,
the caller goes on to make its PAID platform call, and only then does
`ReplaceCampaign` (which does exclude deleted rows) fail. The campaign ends up
mutated upstream with no local record of it — precisely the outcome the
advisory-lock protocol exists to prevent, reached from the other direction.

The EXISTS probe that classifies a no-row claim as 404 vs 412 needed the same
predicate, and needs it for a separate reason: if the two queries disagree, a
soft-deleted row counts as "exists" and a correct 404 degrades into a 412 —
telling the caller to reload and retry a campaign that is gone for good.

Both statements were hoisted out of the function body into named constants,
`claimCampaignVersionQuery` and `claimCampaignExistsQuery`, so the pre-existing
`TestCampaignRepo_ReadsExcludeSoftDeleted` source-inspection test can hold them to
the predicate alongside the other three reads. That test only sees package-level
query constants; leaving the SQL inline is what let the omission through in the
first place. Verified binding by removing the predicate and confirming the failure.
