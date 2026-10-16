# 2026-10-16 — X client: a request that could not be signed or built is NOT_SENT

**Fix** — the sibling-parity gap recorded in
[2026-10-16-LFXV2-2665-google-request-not-transmitted](2026-10-16-LFXV2-2665-google-request-not-transmitted.md):
`internal/platform/twitter`'s `doRequestAbs` had two pre-send exits — a `buildOAuthHeader` failure
and an `http.NewRequestWithContext` failure — that returned an unmarked error. The keyword write
levers (`CreateNegativeKeywordCriterion`, `DeleteTargetingCriterion`) ask `ProbeNotSent(err)`,
which did not recognise it, so the dispatcher's per-keyword default stamped a request X never saw
`REJECTED`.

**The enumeration.** Every exit before `httpClient.Do` on the X client's send paths:

| Exit | Before | After |
| --- | --- | --- |
| `doRequestAbs` context already done at entry | `errRequestContextAlreadyDone` → NOT_SENT | unchanged |
| `doRequestAbs` `buildOAuthHeader` | unmarked → REJECTED | `requestNotSentError` → NOT_SENT |
| `doRequestAbs` `http.NewRequestWithContext` | unmarked → REJECTED | `requestNotSentError` → NOT_SENT |
| `doBodyRequest` (media upload) context already done | `errRequestContextAlreadyDone` | unchanged |
| `doBodyRequest` `buildOAuthHeader` | unmarked (definite "failed") | `requestNotSentError` |
| `doBodyRequest` `http.NewRequestWithContext` | unmarked (definite "failed") | `requestNotSentError` (unreachable once signing succeeded; marked for consistency) |
| pre-send dial failure | `preSendError` → NOT_SENT | unchanged |
| write pacer wait cut short | caller wraps `ErrWriteNotSent` | unchanged |

`downloadStatsFile` (monitor) is an unsigned GET of a public results file with no NOT_SENT
vocabulary and is out of scope.

**The fix.** `requestNotSentError` (`client.go`) unwraps to both `ErrWriteNotSent` and the cause,
so `errors.Is` reaches each, and renders only method, path label and stage — the cause of a URL
parse failure embeds the full request URL. Both stages are deterministic in (method, URL) and the
URL is fixed before the retry loop, so only the first attempt can reach them: unlike Google's
token leg, no attempt guard is needed. `ProbeNotSent` now matches the type, so the connection
probe's metrics arm also drops a sample for a request X never received; `ProbeInconclusive`
already claimed it through its default arm.

**Tests.** `TestPreSendExits_AreNotSent` forces each reachable exit (an unparsable base URL for
signing, a method that is not an RFC 7230 token for building, an unparsable upload base URL) and
asserts `errors.Is(err, ErrWriteNotSent)`, `ProbeNotSent`, not unconfirmed, no URL in the text and
zero requests at a counting server. `TestKeywordWriteLevers_UnsignableRequestIsNotSent` does the
same through both keyword write levers. `TestTwitter_ProbeConnection_UnsignableRequestIsNotAttempted`
is the dispatch-level reach: inconclusive plus `ErrConnectionProbeNotAttempted`, zero requests. A
dispatch-level write-lever case is not constructible: each lever reads the line item through the
same client first, so a URL that cannot be signed fails the read before any write. Mutation-checked:
reverting either `doRequestAbs` exit, the `doBodyRequest` sign exit, the `ProbeNotSent` match, or
`ErrWriteNotSent` in `Unwrap` each fails at least one test.
