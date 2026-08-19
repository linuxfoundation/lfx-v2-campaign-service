# 2026-08-19 — LFXV2-3033: resolved-client cache extended to Reddit and Microsoft

**Update** — `clientCache` was wired into `RedditDispatcher` and `MicrosoftDispatcher`, which
until now built a fresh platform client on every resolve. Because each client caches its OAuth
access token on the INSTANCE, a rebuilt client re-mints the token however cheap the credential
lookup became — so LFXV2-3036's credential cache removed the decrypt but left the token exchange
untouched on these two paths. A fan-out therefore re-exchanged a token per operation.

Both providers now route construction through the SAME `clientCache.buildOnce` Google Ads uses,
rather than a second mechanism: Reddit inside `resolveRedditClient`, Microsoft through a new
`cachedMicrosoftClient` helper shared by the dispatch and toggle paths.

**The invalidation contract was the part that mattered, and it was verified before being
replicated rather than assumed.** `clientCache` entries carry `(connID, version)` and
`resolved.cacheIdentity` supplies them from the row the credential just resolved from, so a client
built from credential version N is dropped the moment the row reports N+1. The `connID` half is
not redundant: `Delete` soft-deletes and `Create` INSERTs a fresh row whose version restarts at
the column DEFAULT of 1, so a disconnect/reconnect produces a DIFFERENT credential at the SAME key
and the SAME version — version alone would keep serving the disconnected account's client and its
live token. Google Ads already got this right, so the wiring replicates it unchanged.

**Concurrency safety was checked per client, not inherited from Google Ads.** Both clients write
only their token cache and in-flight refresh handle after construction, both exclusively under a
mutex (`reddit.Client.mu`, `microsoft.Client.tokenMu`), and neither stashes per-call state on the
receiver. Microsoft was the one worth checking rather than assuming: it is the client with
multi-customer discovery, and a `CustomerID` mutated per call would have made a shared instance
serve one caller's request against another caller's customer. It does not — the customer id is a
per-call argument to `doCustomerRequest`/`accountsInfoForCustomer`. Microsoft's `ListAccounts`
discovery client is deliberately left UNCACHED for a different reason: it is built with a zero
`AccountConfig`, so caching it under the connection's identity would let discovery and dispatch
serve each other's client.

**This is a deliberate partial rollout.** LinkedIn, Meta and X/Twitter are NOT wired, because open
PRs owned those files (cs#148 linkedin, cs#152 meta+twitter, cs#158 meta) and touching them would
have created merge conflicts. They still rebuild per resolve and still re-mint per operation. The
follow-up should reuse this same pattern once those PRs land.

Six tests were added in `internal/dispatch/clientcache_providers_test.go` — reuse, rotation +
reconnect, and cold-key concurrent coalescing, per provider. Each was mutation-verified with a
compiling revert: bypassing the Reddit cache killed the Reddit reuse and coalescing tests,
bypassing the Microsoft cache killed the Microsoft pair, and disabling the `(connID, version)`
guard in `clientCache.get` killed both rotation tests (along with the two pre-existing Google Ads
ones). The rotation tests assert the token-exchange COUNT rather than only pointer inequality, so
a cache that returned a fresh wrapper around a shared token would not pass them.
