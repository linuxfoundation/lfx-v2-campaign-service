# 2026-10-16 — LFXV2-2665: every pre-transmission exit of the Google negative-keyword mutate

**Fix** — PR #346's second bot round raised one more instance of the class the first round raised
three of: an outcome the client could not observe, reported to the caller as a definite platform
rejection. The named instance was a token-acquisition failure. Rather than mark that one exit,
every error exit of `mutateCampaignNegativeKeywords`' path was enumerated and classified, because
fixing the named instance buys exactly one review round.

**The enumeration.** `doRequestValidated` (`internal/platform/googleads/client.go`) has these
error exits on this path:

| Exit | Before | True sentence? |
| --- | --- | --- |
| pre-send dial failure (DNS, connect-time refusal) | `ProbeNotSent` → NOT_SENT | yes |
| `transportError` mid-flight | ambiguous → UNCONFIRMED | yes |
| `apiError` 429 or 5xx | ambiguous → UNCONFIRMED | yes |
| `apiError` 4xx | unmarked → REJECTED | yes — Google did refuse the batch |
| token acquisition failure | unmarked → REJECTED | **no** — the batch never left |
| `marshal request body` | unmarked → REJECTED | **no** — never reported by any bot |
| `build request` | unmarked → REJECTED | **no** — never reported by any bot |

Three false exits, where the review round named one.

**The fix.** `errRequestNotTransmitted`, exported through `RequestNotTransmitted(err) bool`, is
attached at the three returns that happen before `httpClient.Do`. The body-encoding failure sits
above the retry loop, so it is unconditionally the first attempt. The token and request-building
failures are marked **only on attempt 0**: `doRequestValidated`'s loop has exactly one `continue`,
inside the 429 branch gated on `idempotent`, so for the mutate (`idempotent=false`) the loop runs
once and the claim is provable — while for an idempotent call a later attempt follows a request
that already transmitted, and marking it would convert the retry-ambiguity defect tracked in #347
into a confident false negative. `mutateCampaignNegativeKeywords` now asks
`ProbeNotSent(err) || RequestNotTransmitted(err)`.

**Why a second marker rather than widening `ProbeNotSent`.** The same fact has two consumers that
need opposite readings. The metrics/connection-test arm must keep seeing a refused credential as
`ProbeCredentialRejected`, because the operator has to be told to replace it; a token refresh
Google answers 401 would otherwise be claimed as "not sent" and shadow that verdict. The
per-keyword outcome arm needs the other reading: whatever the reason the token could not be
obtained, Google never saw these keywords, so `NOT_SENT` is true there. Widening `ProbeNotSent`
would have fixed the keyword arm by breaking the credential arm.

**Tests.** `TestAddCampaignNegativeKeywords_TokenFailureAfterPreReadIsNotAttempted` serves a token
endpoint whose `expires_in` is inside `tokenExpiryBuffer` (so every call refreshes), runs a
successful `ListCampaignNegativeKeywords` pre-read, then arms the `RoundTripper` to answer the
token endpoint `503`, and asserts `NotAttempted()` and **not** `IsOutcomeUnconfirmed`. It was
confirmed to discriminate: with `|| RequestNotTransmitted(err)` reverted it fails, and the
operator-facing sentence it produces is the false one the fix exists to prevent.
`TestRequestNotTransmitted_OnlyClaimsTheFirstAttempt` pins the `attempt == 0` guard by failing the
token endpoint on its second hit while the API answers `429 Retry-After: 0`, asserting the marker
is absent after a retry. `TestRequestNotTransmitted_IsNotReadByProbeNotSent` pins the separation
itself: a `ErrCredentialRejected` wrapped in the new marker is `RequestNotTransmitted`, is **not**
`ProbeNotSent`, and is still `ProbeCredentialRejected`.

**Sibling parity — one gap found, not fixed here.** `internal/platform/reddit`'s dispatcher arm
(`reddit_negative_keywords.go`) defaults to `NOT_SENT` and refuses only on `IsPlatformRefusal`
(4xx other than 429), so a pre-transmission failure is already classified truthfully there.
`internal/platform/twitter` is the one that carries this class: `CreateNegativeKeywordCriterion`
asks `ProbeNotSent(err)` alone, and `doRequest` has two unmarked pre-send exits —
`buildOAuthHeader` and `http.NewRequestWithContext` — which reach the dispatcher's default and are
stamped `REJECTED`. It is pre-existing, narrower than Google's (X signs OAuth1 locally, so there
is no network token refresh to fail), and belongs in its own change rather than widening this PR,
the same call made for the retry ambiguity in #347.
