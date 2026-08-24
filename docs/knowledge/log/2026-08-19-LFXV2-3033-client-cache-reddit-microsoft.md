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

**Concurrency safety was checked per client, not inherited from Google Ads.** Everything either
client writes after construction is mutex-guarded and neither stashes per-call state on the
receiver. Reddit writes only its token cache and in-flight refresh handle, under
`reddit.Client.mu`. Microsoft has TWO such locks, not one: the token cache and its refresh handle
under `tokenMu`, and the parsed geo-locations snapshot plus its in-flight fetch (`geo.snapshot` /
`geo.inflight`) under the separate `geo.mu` — deliberately separate because a token refresh is a
small JSON round trip while a locations refresh is a multi-MiB download, and one lock would let a
slow file fetch stall every token read. Microsoft was the one worth checking rather than assuming: it is the client with
multi-customer discovery, and a `CustomerID` that VARIED per caller would have made a shared
instance serve one caller's request against another caller's customer. It does not — but not
because the id is request-local. The configured customer is held ON the receiver in
`c.account.CustomerID`, and `doCustomerRequest` reads that field rather than taking a customer
argument. Sharing is safe because `c.account` is an immutable `AccountConfig` fixed at
construction and the cache key pins the connection row id and version, so every caller of a given
cached client is a caller for that same customer. The genuinely per-customer path is
`accountsInfoForCustomer`. Microsoft's `ListAccounts`
discovery client is deliberately left UNCACHED for a different reason: it is built with a zero
`AccountConfig`, so caching it under the connection's identity would let discovery and dispatch
serve each other's client.

**The provider roster now lives in exactly one place.** Three live source comments enumerated
which dispatchers were cached, and wiring Reddit and Microsoft falsified two of them outright:
`credcache.go`'s "wired today only into Google Ads", and `creds.go`'s "which today exists only for
Google Ads … Reddit and Microsoft still rebuild per resolve and still re-mint" — the latter naming
the very two providers this change wired. A third, `clientCache`'s own concurrency paragraph,
presented `googleads.Client`'s mutex as the safety argument for a field typed `any` that now also
holds `*reddit.Client` and `*microsoft.Client`.

Rather than correct three enumerations into three new enumerations that the LinkedIn/Meta/X
follow-up would falsify again, the roster (wired: Google Ads, Reddit, Microsoft; deferred:
LinkedIn, Meta, X/Twitter) and the discovery-path exclusions now live on `clientCache`'s doc
comment ONLY, and the other sites point at it. The concurrency paragraph was restated as the
abstract property every stored client must hold — guards its token cache with a mutex, stashes no
per-call state on the receiver — with the verification delegated to the per-dispatcher `clients`
field comments, plus the explicit warning that a future provider whose client stashes per-call
state must not be wired without changing the client first. `googleads.go`'s `clients` comment
gained the concurrency paragraph its Reddit and Microsoft counterparts already had.

**This is a deliberate partial rollout.** LinkedIn, Meta and X/Twitter are NOT wired, because open
PRs owned those files (cs#148 linkedin, cs#152 meta+twitter, cs#158 meta) and touching them would
have created merge conflicts. They still rebuild a client per resolve — but rebuilding a client is
not re-minting a token, and none of these three re-mints one: Meta and LinkedIn are handed an
already-minted bearer token and do no exchange at construction, and X signs each request with
stored OAuth 1.0a credentials. Their remaining win is allocation reuse, not a saved token
round-trip. The follow-up should reuse this same pattern once those PRs land, subject to the
per-provider concurrency check the roster documents.

Six tests were added in `internal/dispatch/clientcache_providers_test.go` — reuse, rotation +
reconnect, and cold-key concurrent coalescing, per provider. The rotation tests assert the
token-exchange COUNT rather than only pointer inequality, so a cache that returned a fresh wrapper
around a shared token would not pass them.

**A rotation-test fixture agreed with itself, and the mutation that exposed it is worth keeping.**
The claim above that disabling the `(connID, version)` guard "killed both rotation tests" was
FALSE as first written. Deleting only `|| entry.connID != connID` from `clientCache.get` — a
compiling mutation that leaves `credCache` intact, so just the CLIENT cache loses its row-identity
check — left all six new tests green; only the pre-existing
`TestClientCache_ReconnectAtSameVersionRebuildsClient` failed. Two things were doing the work
instead of the assertion:

- The reconnect step changed the account id and credential ALONGSIDE the row id
  (`"conn-1"/"t2_acct"` → `"conn-2"/"t2_other"`), so the upstream credential cache and the
  differing plaintext forced the rebuild on their own.
- More subtly, and the part that survived a first fix attempt: the reconnect landed at **version 1
  while the rotation step above had already cached version 2**. The entry therefore missed on the
  VERSION comparison and `connID` was never consulted at all — holding the credential and account
  id constant was necessary but NOT sufficient.

The fix is to reconnect at the SAME version the cached entry carries (version 2), changing only
the row id, which makes `connID` the sole discriminator. Both rotation tests now fail under that
mutation with the intended message. The `c3.AccountID()` assertions were dropped: once the account
id is held constant they no longer discriminate.

The general lesson is the one that generalises past this cache: a reconnect fixture has to hold
**every** component of the cache identity constant except the one under test, and "version
restarts at 1" is a fact about the schema, not a safe fixture — what matters is the version the
CACHED entry holds at that moment.

The other four mutations do bind as described: bypassing the Reddit cache killed the Reddit reuse
and coalescing tests, bypassing the Microsoft cache killed the Microsoft pair, and forcing
`clientCache.get` to always miss failed four of the six with their intended messages.
