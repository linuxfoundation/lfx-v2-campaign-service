# 2026-10-16 — LFXV2-2665: Google negative-keyword outcome classification and dedupe key

**Fix** — Two defects raised on PR #346, both the same class as the 80-rune text-limit fix in the
same branch: an outcome the client could not actually observe was reported to the caller as a
definite platform rejection.

**A mutate that never left this process read as a platform rejection.**
`mutateCampaignNegativeKeywords` (`internal/platform/googleads/negative_keywords.go`) classified a
`doRequest` error through `createOutcomeAmbiguous` alone. A pre-send transport failure — a DNS
failure, a connect-time refusal — is not ambiguous and is not a rejection: the batch provably never
reached Google, so the criteria cannot exist. Unmarked, that error carried no outcome at all and
the dispatcher's DEFINITE arm reported every keyword in the batch as `REJECTED` over HTTP 200.
`ProbeNotSent` (`internal/platform/googleads/probe.go`) now runs **before** the ambiguity check and
wraps the error in `notAttemptedNegativeKeywordError`, because it proves the stronger fact; the
ambiguity check keeps everything `ProbeNotSent` does not recognise, which is the cheap direction —
a cancellation landing after transmission is indistinguishable from one landing before it.
`notAttemptedNegativeKeywordError`'s docblock said "the one failure" was an already-cancelled
context; there are now two, and it says so.

**The client's dedupe key did not agree with the dispatcher's.** `validateKeywordList`
(`internal/platform/googleads/targeting.go`, shared by `validateNegativeKeywords` and the create
path's `validateKeywords`) keyed on `matchType + NUL + strings.ToLower(text)`, while
`internal/dispatch` keys every negative-keyword comparison on `textfold.Key`. The two folds
disagree in **both** directions — `strings.ToLower` collapses `İ` onto `i` where simple folding
keeps them distinct, and keeps `ΟΣ` apart from `ος` where simple folding joins them. A
dispatcher-approved batch of two could therefore shrink to one operation here: Google would
acknowledge one id, the dispatcher — still expecting two — would answer UNCONFIRMED, and one
exclusion would be silently never applied, identically on every retry. The key is now
`textfold.Key`, the one case-insensitive key every negative-keyword comparison uses.

**Tests.** `TestAddCampaignNegativeKeywords_PreSendDialFailureIsNotAttempted` serves the pre-read
from an `httptest` server and fails only the `campaignCriteria:mutate` with a real
`*net.OpError{Op: "dial", Err: ECONNREFUSED}` through a custom `RoundTripper`, asserting
`NotAttempted()` and **not** `IsOutcomeUnconfirmed`.
`TestValidateNegativeKeywords_FoldsLikeTheDispatcher` asserts cardinality — what the dispatcher
pairs on — over both divergent pairs plus plain ASCII case and the match-type half of the key; it
was confirmed to fail on both divergent rows under the old `strings.ToLower` key, so it pins the
fold rather than merely passing beside it.

**Not fixed here, filed separately.** The googleads client's five idempotent `doRequest(…, true)`
call sites (`budget_update.go`, `campaign.go`, `adgroup_ad.go` ×2, `bid_update.go`) cannot tell a
first-attempt failure from one that failed after a retry already put the request on the wire, so a
retried-then-failed mutate can be reported as definite. `internal/platform/microsoft` solves this
with `doRequestCounted` returning a retry count and `retriedUnconfirmedError`; googleads has no
counted variant. It is pre-existing and class-wide, so fixing one call site would be the instance
fix, not the class fix.
