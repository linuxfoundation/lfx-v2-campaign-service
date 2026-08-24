# 2026-08-19 — LFXV2-3281: a 401 is classified on every arm, not just the readable one

**Fix** — The preceding change made a mutating 401 outcome-ambiguous so a create whose
401 may have followed a committed upstream write retains its dispatch claim. That only
held on the arm that successfully READ the response body. `doRequest` leaves a non-2xx
response by three exits, and two of them return earlier: a body-read failure (a
mid-flight reset after the status line is on the wire) and a body over
`maxResponseBytes`. Both returned a bare `*apiError`, so a 401 arriving on either one
never reached the 401 arm at all.

That cost two distinct things. `createOutcomeAmbiguous`'s `apiError` arm returns true
only for 3xx, 429 or >=500, so a mutating POST answered 401 fell through every arm and
classified as a DEFINITE failure — `CreateCampaign` returned a nil result, the
dispatcher took its `result == nil` branch and RELEASED the claim, and a retry could
duplicate a campaign group LinkedIn may already have committed and be billing. Separately,
only the readable-body arm called `invalidateAccessToken()`, so a token LinkedIn had
already rejected survived in cache and was replayed by the next caller.

Fixed by class rather than by the two named lines. `expiredCredentialsError` is now the
single construction site for a response-arm 401, in `token.go` beside
`isTokenExpiryResponse`: it returns `nil` when the status is not an expiry, so each arm
uses it as a guard ahead of its generic `apiError` return. All five arms that can observe
a non-2xx now route through it — `doRequest`'s readable-body, read-failure and over-cap
exits, plus `metrics.go`'s read-failure and over-cap exits. The body argument is optional
(`""` where none was obtained), since `isTokenExpiryResponse` treats an unparseable body
as an expiry and the 401 status is itself the operative signal.

Both options considered in review were needed, and the shared site delivers both at once:
adding 401 to the `apiError` status list would have kept the ambiguity but left the
operator with an opaque upstream error and the dead token still cached, so returning a
proper `credentialsExpiredError` with `Method`+`StatusCode` is what makes the "reconnect"
message correct AND restores the invalidation.

The method gate is deliberately unweakened: a GET 401 and every pre-send expiry
(`Method == ""`) stay DEFINITE, which the enumeration confirmed is right on every other
exit path too — the pre-send, transport, decode and redirect arms were already correct.
`skipBody` (status updates) was NOT exempt and is covered: its early returns are gated on
a 2xx, so a non-2xx falls into the same arms, and the cascade tunnels `PARTIAL_UPDATE`
over POST. `metrics.go` needed only the invalidation half — its hard-coded GET method
means the gate correctly keeps an analytics 401 definite.

Each newly-classified path is mutation-verified with a compiling revert: reverting either
`doRequest` arm fails four tests including the end-to-end `result = nil` claim-release,
and reverting the `metrics.go` arms fails the token-exchange count. A sweep of every
existing `StatusUnauthorized` test site confirmed none asserted the old bare-`apiError`
behaviour — all serve small readable bodies or are pre-send.
