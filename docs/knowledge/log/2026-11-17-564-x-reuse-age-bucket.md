# 2026-11-17 — #564 X reuse checks the live age bucket and gender first

**Fix** — On a REUSED PAUSED line item, an X create read the live criteria and posted the
missing ones (`missingCriteria`). When the line item already held a different `AGE` bucket, the
requested bucket was queued as a second `AGE` criterion, which X refuses ("a single AGE bucket
per line item"). The criteria ahead of it had already been posted by then, so the targeting was
left half-changed and needed reconciliation. `GENDER` had the same shape (one value per line
item) and gets the same fix.

- `conflictingSingleValue` (`create_targeting.go`) runs on the live criteria the reuse path
  already reads, before any criterion write. A live, non-deleted `AGE` or `GENDER` with a
  different value or operator stops the create there, in the same form as the other reuse stops
  (`Targeting NOT applied to reused line item …`, wrapping `ErrCreateTargetingIncomplete`):
  nothing written, nothing authored or promoted.
- An identical live bucket or gender is kept and not re-sent; the other criteria still go out.
- A fresh line item is unchanged: it is not read and gets its `AGE` and `GENDER` criteria as
  before.
- Tests: `TestCreateCampaign_ReusedLineItemAgeAndGenderMustMatch` (matching bucket and gender,
  matching bucket with gender missing, deleted different bucket, conflicting bucket, conflicting
  gender, fresh line item). The conflict cases fail without the fix.
- Docs: `twitterConfig.ageBucket` / `gender` in `docs/api-catalog.md`, and the reuse steps in
  [internal/platform/twitter](../code/internal-platform-twitter.md).
