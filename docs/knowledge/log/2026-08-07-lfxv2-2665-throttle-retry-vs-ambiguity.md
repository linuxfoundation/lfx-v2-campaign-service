# 2026-08-07 — The retry loop and the classifier disagreed about what a throttle means

**Update** — Closed a review finding on PR #79 (`internal/platform/meta/client.go`
and `client_test.go`).

The previous change taught `createOutcomeAmbiguous` that a throttle — HTTP 429 **or**
the more common HTTP 400 carrying a Graph rate-limit code — is UNCONFIRMED, because
Meta may have committed the node before reporting the limit. That premise was never
carried back to the retry loop, which sat a few hundred lines below acting on the
opposite one: `doRequest` retried a throttled request up to `retryMax` times
regardless of method, and the four creates (campaign, ad set, ad creative, ad) all
went through it.

So one `CreateCampaign` call could POST the same create up to four times. The
find-by-name idempotency this PR is built on runs at the **start** of the flow, not
between retry attempts — it cannot see a duplicate created inside the call it is
already past. The result would be two campaigns with the identical name, one of them
orphaned and invisible to every later pass. That is the exact outcome the ambiguity
machinery exists to prevent, defeated by the retry sitting upstream of it.

**Fix.** `doRequest` and a new `doCreate` are thin wrappers over a shared `do` that
takes `retryThrottle`. `doCreate` clears it; the four create call sites use it. The
flag is cleared by zeroing `throttled` rather than branching at the retry site, so the
unreadable-body short-circuit stays correct too — for a create there is no "we're
about to retry, the body is discarded anyway" case, so a throttled response with an
unreadable body must be consumed as final and carry its status, or a 400-coded
throttle would reach `createOutcomeAmbiguous` stripped of what it classifies on.

**Scoped to creates, not to POST.** The status-update POSTs assert a desired state
rather than creating a node, so repeating them changes nothing; they keep the retry.
A negative test pins that, so a future "just don't retry POSTs" simplification fails
a test rather than quietly costing availability.

The cost of the fix is one lost in-call retry when the throttle really was a clean
pre-commit rejection: the flow returns UNCONFIRMED and the next run adopts or creates
once. The cost of not fixing it is a duplicate no later pass can tell from the
original. Revert-verified: restoring the retry makes the throttled create hit the
server 4 times instead of 1, for all three throttle shapes.
