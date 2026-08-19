# 2026-08-18 — LFXV2-3033/3036 shared credential resolver cache

**Update** — every dispatch operation re-resolved the platform credential and re-minted an OAuth
access token. `resolveGoogleAdsClient` (and the Twitter/Reddit/Meta/LinkedIn/Microsoft/HubSpot
equivalents) built a brand-new client per call, and each client owns its OWN token cache — so a
dashboard polling metrics performed a credential decrypt plus a token exchange per read, with no
reuse even for the same project seconds apart. Done ONCE at `credsSource`, the single choke point
all sixteen `resolve`/`resolveOwned` call sites already funnel through, rather than per platform.

**The cache holds decrypted credentials, not connection rows — and that is what settles both
invalidation and multi-replica.** Every resolve still issues the repository `Get`, a single-row
lookup on the partial unique index, and the row it returns is what the cached entry is validated
against: the entry records the row `id` and `version` it was decrypted from, and
`version = version + 1` runs in every mutating statement in `ConnectionRepo` (`update`,
`SetCredential`, `Delete`). A rotated, re-pointed or revoked credential therefore cannot match,
and the entry is dropped on the spot.

Caching the row instead would have needed an eviction hook on every mutation path, and an
in-process hook cannot evict on the OTHER replicas — the pod serving the next request need not be
the pod that handled the write, so a revoked credential would keep being served elsewhere until
some TTL expired. Validating against a freshly read version removes that failure mode instead of
bounding it: the write lands in Postgres, which every replica reads, so the first resolve after a
rotation misses on EVERY replica at once. **There is no staleness window to defend and no shared
cache store to operate.** The price is one indexed SELECT per resolve, which the service already
paid; the decrypt and the downstream token exchange are the costs actually removed.

**Caching the credential was only half of it — the token exchange needed the CLIENT.** The first
version of this change cached the decrypted credential and claimed the OAuth exchange with it.
That claim was false, and a token-endpoint count proved it: five resolves produced FIVE token
hits. The access token is cached on the platform client INSTANCE
(`internal/platform/googleads/client.go`), so rebuilding the client per resolve re-mints the token
no matter how cheap the credential lookup became — and a decrypt-count assertion cannot observe
that at all. `GoogleAdsDispatcher` now caches the built client under the same (row id, version)
identity, which is what LFXV2-3036 actually asks for. Raised by Copilot on the first push, and it
was right; pinned by `TestClientCache_ReusedClientPerformsOneTokenExchange`, which counts hits on
a real HTTP token endpoint rather than decrypts.

The client is validated exactly like the credential, because a client is precisely as stale as the
credential it was built from: without that, the client cache would reintroduce through the back
door the very failure the credential cache prevents — a revoked credential still authenticating,
held alive inside a cached token. `TestClientCache_RotatedCredentialRebuildsClient` asserts a
DIFFERENT client instance and a second token exchange after a rotation.

**A warm-key cache still leaves the cold-key burst.** Caching the client fixed repeated resolves
but not simultaneous first ones: N callers arriving together on a cold key all miss, each builds a
client, and each mints its own token — 16 exchanges across 16 callers, which is no better than
having no cache, and is exactly what a dashboard loading several panels produces. Client
construction is now coalesced through singleflight on the same identity. Raised by Copilot in a
SUPPRESSED comment (the review said "no new comments" inline), which is the second time on this PR
that the suppressed block held the substantive finding. `TestClientCache_ColdKeyConcurrentCallers
ShareOneTokenExchange` pins it; the sequential reuse test could not see it.

**One cache, not eight.** The eight adapter constructors each call `newCredsSource`, so allocating
a cache per source would have produced eight private caches — reuse never crossing adapters (HubSpot
has two consumers), the plaintext bound silently multiplied by eight, and the provider key component
dead. `newCredsSource` now resolves a cache shared per `(repo, encryptor)` pair: as wide as the
credential domain and no wider, since the same key means a different credential under a different
database or key. Also caught by review; pinned in both directions by
`TestCredCache_IsSharedAcrossDispatchers` and `TestCredCache_DistinctBackendsDoNotShareEntries`.

**Key is (project, provider), and the schema proves that is exact.** Migration 000001 creates
`uq_<provider>_connections_project ON <table> (project_id) WHERE status <> 'deleted'` for all
seven provider tables, so a project cannot have two live connections for one provider. `Delete`
soft-deletes, leaving tombstones outside that index. The key uses the RESOLVED scope, so a project
on the LF system fallback caches under `model.SystemProjectID` and can neither read nor poison a
project-owned entry.

**The row id is checked alongside the version, because a reconnect restarts the version.** `Delete`
soft-deletes and `Create` INSERTs a fresh row at the column `DEFAULT 1`. A project that dispatched
once while still at version 1, disconnected, and reconnected a DIFFERENT ad account inside the TTL
would otherwise present the same key at the same version — and be served the credential of the
account it had just disconnected, silently, against the wrong account. Caught in review; pinned by
`TestCredCache_ReconnectAtSameVersionIsNotServedFromCache`.

**TTL is a memory-residency bound, not a staleness bound.** Correctness comes from the id+version
check; the 5-minute TTL and the 512-entry cap exist because entries hold DECRYPTED credentials, and
an unbounded cache would converge on "every project's plaintext, resident forever". It is a SLIDING
idle window: a credential polled more often than the TTL stays resident for as long as it keeps
being used — for the pod's lifetime, in the dashboard case. That is the intended trade, since
continuous reuse is the point and an absolute cap would force a re-decrypt mid-poll without
reducing exposure (the credential is in memory throughout either way). An earlier draft of this
entry claimed a flat 5-minute bound, which the code does not provide; it now says which bound it is.

**Concurrency: N callers, one exchange.** Misses are coalesced through `singleflight` keyed by
(project, provider, **id**, **version**) — the id and version are load-bearing, not decoration.
Without them a caller that had read the ROTATED (or reconnected) row could join a leader's earlier
flight and be handed the superseded credential: the "revoked credential still authenticates"
outcome, reached through the coalescing path rather than the cache.
`TestCredCache_SingleflightDoesNotShareAcrossVersions` pins it, and bounds its wait, because the
version-independent mutant HANGS rather than failing.

Getting the coalescing TEST right took three attempts, all measured rather than reasoned: waiting
for the first decrypt to begin (3 failures in 12 runs; independently reproduced at 11/40),
signalling before calling resolve (decrypts = 17), and a rendezvous inside the repository read
(decrypts = 18). None of them orders the followers against the leader's COMPLETION, which is what
the assertion needs. The test now holds the leader inside the decrypt until the followers are
observably parked in the singleflight group, and passes 100/100 under `-race`.

**Callers get a per-call copy.** `resolve` stamps `fromSystem` on the value it returns, which is a
property of how that call resolved rather than of the credential. Sharing one `*resolved` made
that a write race under `-race` and let one path's attribution appear on another's — misrouting a
defect between the caller's 400 and the 500 that pages whoever installed the LF credential. The
copy is shallow deliberately: the plaintext bytes and providerConfig map are read-only to every
consumer, so deep-copying would cost an allocation per resolve to defend a mutation nobody makes.

**A third vacuous test, found by mutation rather than by review.** The cross-project client
isolation test passed with the PROJECT stripped from the cache key, because its two fixtures also
had different row ids — so the id check caught the collision and the component actually under test
was never exercised. The fixtures now share an id (impossible in Postgres, constructed to isolate
the one component), and the mutant fails. The pattern is worth naming: a test whose fixtures differ
in more ways than the property it names will pass for the wrong reason.

**A 98%-binding test is a broken test.** The first cold-key coalescing test drove
`resolveGoogleAdsClient` behind the repository barrier, which releases all callers at one instant
but does NOT order them against client CONSTRUCTION — so the leader could populate the entry before
a straggler reached the cache. Measured: the no-coalescing mutant SURVIVED 1 run in 60. That is the
same defect the credential barrier had twice, in a new place, and a mutant that survives one run in
sixty is a broken implementation that passes CI. Replaced with a test that drives `buildOnce`
directly and holds the build closure in flight: the mutant now fails 30/30.

**A flaky test is a finding, not noise.** The first version of the coalescing test failed
intermittently, and the reflex reading — "concurrency test, just rerun it" — would have shipped a
test that reddens CI on unrelated PRs. The flake was in the harness, not the cache, and the three
measured failure modes above are recorded in the test itself so the next person does not
re-derive them.

**Two mutation-testing findings, both kept rather than papered over.** A first version of the
attribution test checked a project-owned resolve on a DIFFERENT cache key, which cannot observe
the shared-struct defect at all — distinct keys never share an entry — so removing the copy passed
it. `TestCredCache_SystemScopeResolvedDirectlyIsNotTaggedFromSystem` resolves
`model.SystemProjectID` as its OWN project, hitting the very entry the fallback's stamp polluted,
and does kill that mutant. Separately, an `invalidate()` call on the decrypt-failure arm survived
its revert: `decryptConn` only runs on a miss, and a miss caused by a version change has already
deleted the entry, so the call was unreachable defence-in-depth. It was DELETED, along with the
now-unused method, rather than kept as code no test can justify.

Credentials still never reach a log line, an error string or a persisted field: the cache stores
plaintext in memory only, and no arm added here logs the decrypt cause (see the notes in
`resolveConn`, which this change preserves).

**The client cache keyed on the CALLING project, not the resolved scope.** `credsSource.resolveConn`
already caches the decrypted credential under `model.SystemProjectID` when a project runs on the LF
system fallback, precisely so that every project sharing that one row shares one entry.
`resolved.cacheIdentity` did not: it built the key from the projectID passed in, so each fallback
project got its OWN cached client for the SAME system row. Because the OAuth access token is cached
on the client INSTANCE, that meant one token exchange per project — the exact per-call exchange the
client cache exists to remove. Measured with a counting token endpoint: five resolves of one system
row across three projects produced THREE token exchanges; after the fix, one. `cacheIdentity` now
derives the scope from `fromSystem`, making the client cache exactly as wide as the credential
cache and no wider — a project-owned row still keys on its own project, so no project can be served
another project's client. Pinned by `TestClientCache_FallbackProjectsShareOneSystemClient`, which
counts token-endpoint hits rather than decrypts, because a decrypt count cannot observe the token.

**The singleflight did not re-check the cache inside the flight.** `singleflight` coalesces only
callers whose `Do` calls OVERLAP. A caller that missed `get()` and was then descheduled could enter
`Do` after the leader's flight had already completed and populated the entry, and would start a
fresh flight and decrypt a credential that was already in the map. Measured: five CONCURRENT
resolves cost TWO decrypts, not one, and a caller entering `Do` after a completed flight cost a
second decrypt every time. The flight closure now re-reads the cache under the same (row id,
version) validation as any other read. That validation is what keeps the re-check from becoming a
staleness hole: a straggler whose own fresh row read saw a SUPERSEDED version still misses and
decrypts, pinned by `TestCredCache_StragglerWithSupersededVersionStillDecrypts` — which fails
against a mutant that re-checks without validating.

**The stale comments were corrected rather than left to drift.** Two comments and one concept
section still described the singleflight key as `(project, provider, version)` after the row id was
added, and `internal-dispatch.md` still stated that every resolve performs `Get` then `Decrypt`.
The `Get` does still run on every call — it is what the entry is validated against — but the
decrypt is now skipped on a hit, which is the whole point of the change.

**`reflect.Type.Comparable()` does not mean the VALUE can be a map key.** `sharedCredCache`
guarded the registry insert with a type-level check, and the guard's whole promise is that an
implementation it cannot key gets its own private cache rather than crashing. A type-only check
does not deliver that: comparability of an INTERFACE field is decided by its dynamic value, so
`struct{ tag any }` holding a `[]int` reports `Comparable() == true` and then panics with "hash of
unhashable type" at the insert. Confirmed in isolation before changing anything — the reflect call
returns true and the map insert panics on the very next line. Both `connReader` and
`domain.Encryptor` are interfaces any caller may implement, so the shape is reachable from outside
this package, and the panic would land on a dispatch path at the first resolve rather than at
construction. `isComparable` now also asks `reflect.Value.Comparable()`, which answers the
value-level question. Pinned by `TestSharedCredCache_UnhashableValueFallsBackInsteadOfPanicking`,
which fails with the original panic when the value check is reverted.

**A comment claimed a token saving the credential singleflight does not provide.** The coalescing
comment in `resolveConn` said N callers "receive the SAME `*resolved`" and therefore share one
downstream token exchange. Neither half is true: every caller receives its own `clone()` (which is
deliberate — `fromSystem` is written per call), each builds its own platform client, and the OAuth
token is cached on the client INSTANCE. Coalescing the decrypt therefore changes nothing
downstream by itself. This is the SAME conflation that made this PR's original central claim false
— caching the credential was measured by decrypt count and assumed to have removed the token
exchange — reintroduced in prose after the code was fixed. The comment now guarantees one DECRYPT
and nothing more, and points at `clientCache` as the thing that collapses the token exchange. The
identical overstatement in `credcache.go` and in the `internal-dispatch` concept is corrected the
same way, including the scope limit: client reuse is wired into Google Ads only, and Reddit and
Microsoft still rebuild per resolve and still re-mint their tokens.

The lesson worth keeping is that the comments outlived the defect they described. A claim that was
merely optimistic when written became false when the code around it was corrected, and prose is
not covered by any test — so an overstated guarantee survives every gate. Both fixes here were to
sentences, not to logic, and the second one would have re-taught the next reader exactly the wrong
model of where the token exchange is saved.
