# 2026-08-24 — LFXV2-3033 the cached Microsoft client's account is receiver state, not a per-call argument

**Fix** — the client cache's safety argument was recorded incorrectly in five places, and the
wrong version was the one a future provider would have copied.

Four source comments and one knowledge concept said a shared `microsoft.Client` is safe to share
because "the customer id travels as a per-call argument (`doCustomerRequest` /
`accountsInfoForCustomer`) rather than being stashed on the receiver". `doCustomerRequest` takes
no customer parameter. Its signature is
`(ctx, method, path string, body any, idempotent bool)`, and it reads `c.account.CustomerID` —
receiver state, set once at construction. The account headers are read the same way
(`CustomerAccountId` from `c.account.AccountID`).

**The conclusion was right and the proof was false**, which is the dangerous combination: sharing
IS safe, but for a different reason. `c.account` is an IMMUTABLE `AccountConfig` fixed at
construction, and the cache key pins the connection row id and version, so every caller that
reaches a given cached client is a caller for that same account. The genuinely per-customer path
is `accountsInfoForCustomer`, whose `ListAccounts` client is built with a ZERO `AccountConfig`
and deliberately BYPASSES the cache — that bypass is what keeps the immutability argument true,
so it is load-bearing rather than an optimisation.

Why it matters beyond prose: the roster comment is explicitly the single source of truth a future
provider consults before joining the cache. "Is the id per-call?" and "is the config immutable?"
are different questions, and a provider that answered the first one would have been wired on a
test that never applied to it.

The same sweep corrected a second false claim in the roster: that the three unwired providers
"re-mint a token per operation". Rebuilding a client is not re-minting a token. Meta and LinkedIn
are handed an already-minted bearer token and perform no exchange at construction
(`meta.Credentials.AccessToken`, and `linkedin.Client` "holds no mutable state"), and X signs
each request with stored OAuth 1.0a credentials. The win for those three would be allocation, not
a saved round-trip — and X documents its client as safe for SEQUENTIAL USE ONLY, so the roster as
written invited exactly the unsafe change it exists to prevent.

Pinned by `TestClientCache_MicrosoftAccountIsReceiverStateNotPerCall`, which asserts the header
the SERVER received across two separate resolves rather than reading the field back off the
client. Mutation-verified: making the account header vary per call (the behaviour the old comment
described) fails the test.
