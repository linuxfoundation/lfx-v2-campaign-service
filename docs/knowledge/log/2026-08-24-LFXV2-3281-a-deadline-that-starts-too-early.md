# 2026-08-24 — a per-attempt deadline that starts before the token it waits for

**Fix** — LinkedIn request budget: `doRequest` and `doAdAnalyticsAttempt` both opened the
30-second per-attempt `context.WithTimeout` and then resolved the bearer token *on that same
context*. Every refresh-capable client performs an OAuth exchange on its first request — a
property `accessTokenValue`'s own comment states — so the exchange was charged against the
request's budget. An exchange that succeeded near the bound left the actual LinkedIn call
almost no time, and a healthy, successfully-refreshed credential still failed.

The ordering is the whole defect, and it was present at two sites, so the fix is a single
constructor both now route through: `authorizedAttempt` acquires the token from the PARENT
context and only then opens the attempt deadline. The exchange is already independently
bounded — `token.go` runs it under its own `requestTimeout` on a `context.WithoutCancel`
context — which is what makes acquiring on the parent safe rather than unbounded. Google Ads
and Microsoft already had this shape; LinkedIn was the outlier.

Routing both paths through one constructor is the point rather than a tidy-up: a future call
site cannot reintroduce the nested budget without deliberately bypassing the only helper that
builds an authenticated attempt.

Proving it needed care. An HTTP request does not transmit its context deadline, so a server
handler's context never carries one — an assertion there passes vacuously. The test taps the
client's `RoundTripper` instead and compares the two observed deadline VALUES: sibling budgets
put the API deadline strictly AFTER the exchange deadline, while a nested budget cannot, since
a child context never outlives its parent. That ordering is exact and machine-speed
independent, so nothing is asserted with a sleep or an elapsed-time threshold.

Mutation-checked both ways: reintroducing the nested budget inside the helper failed both
tests, and replacing the exchange's own `WithTimeout` with `WithCancel` failed them too — so
the guard also pins the precondition that makes the fix correct, rather than only its result.
