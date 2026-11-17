# 2026-11-17 — #564 X part 2c: negative keyword adds share the positive add's lock and 60-criterion cap

**Fix** — from the Greptile, Copilot and lfx-one reviews of PR #627:

- `AddKeywordTargeting` held the line item's targeting lock while counting live criteria for the
  60 cap, but `AddNegativeKeywords` wrote to the same line item without the lock and without
  counting against the cap. From 59 live criteria, overlapping positive and negative adds could end
  at 61, and a negative add alone could exceed 60.
- `AddNegativeKeywords` now takes the same per-line-item lock (`lockTwitterTargeting`), released by
  the shared `settleTwitterTargetingLock` (kept for the cooldown after any UNCONFIRMED item), and
  enforces the same cap through the shared `checkTwitterCriteriaCap`: live criteria of every type
  plus the negatives the request would send over 60 is `ErrKeywordTargetingLimit`. The negative
  endpoint maps it to 409 (nothing added), and on X a contended lock now names the line item rather
  than an ad group and its revision.
- Tests: `TestTwitter_OverlappingPositiveAndNegativeAddsStayUnderTheCap` (both orders, the first
  POST held open), `TestTwitter_AddNegativeKeywords_CountsAgainstTheCriteriaCap`,
  `TestTwitter_AddNegativeKeywords_HoldsThePerLineItemLock`, and two error-mapping rows; each fails
  without the fix.
- Docs: the api-catalog negative-add row, `keyword-targeting-reddit-x.md` and
  `internal-dispatch.md` say both adds share the lock and the cap.

Not verified live: no X call was made.
