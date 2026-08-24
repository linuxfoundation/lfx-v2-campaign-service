# 2026-08-19 — LFXV2-3281 metrics 401 classification and token body-read redaction

**Fix** — Two real defects the bot reviewers found on the refresh PR. The first meant the PR did
not repair the exact production path it was written for; the second was a live credential leak.

**The metrics path never classified a 401, so the incident this PR targets was not closed.**
`GetCampaignMetrics` does not go through `doRequest` — it uses
`makeAdAnalyticsRequest`/`doAdAnalyticsAttempt`, which had its own non-2xx arm returning a bare
`apiError`. Resolving the token through `accessTokenValue` (already done) only covers a token
the client KNOWS is expired, and that is not the failing case: stored LinkedIn credentials carry
no `AccessTokenExpiresAt`, so a bearer-only row has a ZERO expiry, nothing predicts it, the token
is sent, and LinkedIn answers 401. Revocation is the same shape and carries no advance notice.
The dispatcher's `ReadMetrics` re-tag (`errors.Is(err, linkedin.ErrCredentialsExpired)`) could
therefore never match on this path, and the monitor kept producing the opaque 500 the ticket
opened for. `doAdAnalyticsAttempt` now classifies a 401 through `isTokenExpiryResponse`,
invalidates the cached token and returns `credentialsExpiredError`, mirroring `doRequest`'s arm.
The response body is read for the `serviceErrorCode` signal and discarded, never retained on
`apiError`.

**A non-401 must not be reclassified.** Reporting a 500 as an expired credential sends an
operator to reconnect a working connection, so the classifier stays keyed on the status.

**The token body-read error leaked the request body.** `fetchToken` wrapped the raw
`buf.ReadFrom` cause with `%w`. `WithHTTPClient` accepts an arbitrary `RoundTripper`, so the
RESPONSE is as caller-controlled as the transport error, and its `Body.Read` error text can echo
the request body — which on this exchange carries `client_secret` and the refresh token. The
transport arm already redacted via `redactHTTPDoError`; this arm bypassed that guarantee, and
these errors are persisted into a campaign's `Steps`, so the leak would be durable. It now goes
through the package's existing `redactBodyReadError`, which rebuilds the cause from its
classification and preserves `context.Canceled`/`DeadlineExceeded` so callers can still use
`errors.Is`.

**Mutation-tested, both.** Disabling the metrics 401 arm still compiles and fails three tests,
with the diagnostic rendering the exact opaque `LinkedIn API GET adAnalytics -> 401` the change
removes; the covered cases are an expired bearer with unknown expiry, a revoked token with no
subcode, a 500 that must NOT be reclassified, and cache invalidation proven by a second read
re-exchanging. Restoring the raw `%w` on the body read fails the leak test with
`client_secret=client-secret&...&refresh_token=stored-refresh-token` rendered verbatim across two
error layers — the walk checks every layer, not just the outermost `Error()`, because a
credential-bearing wrapper left in the chain is reachable by any structured logger.
