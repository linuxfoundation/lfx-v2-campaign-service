# 2026-08-07 — The throttle guard missed the shape the throttle usually takes

**Update** — Closed a review finding on PR #79 (`internal/platform/meta/client.go`
and `client_test.go`).

The previous change made an exhausted 429 ambiguous rather than a clean rejection. It
checked the HTTP status alone — and **the HTTP status does not identify a Meta throttle.**
Meta reports rate limiting as a 429 OR, at least as often on the Marketing API, as an
HTTP **400** carrying a Graph error envelope whose `code` is one of the known rate-limit
codes: 4 (application request-limit), 17 (user request-limit), 32 (page-level), 341
(temporary app-level), 613 (ad-account), 80004 (business-use-case).

`doRequest` already knows this — `graphRateLimitCodes` is what makes it retry the 400
form — and it preserves that code on the `APIError` it returns once the budget is spent.
So the guard and the retry loop disagreed about what a throttle is: the retry loop treated
the 400 form as retryable, and the guard then classified the exhausted result as a
definite rejection, releasing the claim on a create that may have committed. The guard
missed its more common case while appearing to cover the category.

`createOutcomeAmbiguous` now checks `graphRateLimitCodes[ae.Code]` alongside the 429.

The table gained a code-driven half: the six throttle codes are ambiguous, and 100
(invalid parameter), 190 (invalid access token), and 0 (no Graph envelope at all) stay
definite rejections — the negative cases matter, since treating every 400 as ambiguous
would make the whole classification useless. Revert-verified: restoring the status-only
check fails all six throttle codes.
