// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"reflect"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// credCacheTTL bounds how long a decrypted credential may live in memory after its last
// use. It is a MEMORY-RESIDENCY bound, not the staleness bound: correctness against a
// rotated or revoked credential comes from the version check in credsSource.resolveConn,
// which runs on every call (see credCache). Nothing is served without first reading the
// row's current version, so a stale entry is detected on the very next resolve regardless
// of this value and regardless of which replica evicted.
//
// Five minutes is therefore chosen for exposure, not for freshness: it is long enough to
// cover a metrics dashboard's poll interval and a dispatch burst (the reuse this change
// exists for), and short enough that an IDLE project's plaintext does not sit in the heap for
// the life of the pod.
//
// It is a SLIDING idle window, not an absolute cap: every hit refreshes lastUsed, so a
// credential polled more often than the TTL stays resident for as long as it keeps being used
// — for the pod's lifetime, in the dashboard case. That is deliberate, because continuous
// reuse is exactly what this cache is for and an absolute cap would force a re-decrypt mid-poll
// for no security gain: the credential is in memory throughout either way. What the window
// bounds is a credential that has STOPPED being used, which is the one an absolute cap and a
// sliding cap treat identically.
const credCacheTTL = 5 * time.Minute

// credCacheMaxEntries caps the number of live entries so the cache cannot grow without
// bound as projects are added. The bound matters because every entry holds DECRYPTED
// credential bytes: an unbounded cache would converge on "every project's plaintext,
// resident forever", which is the opposite of what bounding the lifetime is for.
//
// On overflow the cache evicts the entry with the oldest last-use, which for this access
// pattern (a project polls or dispatches in bursts, then goes quiet) is the entry least
// likely to be wanted next. Eviction is never a correctness event — it only costs a
// decrypt — so an approximate policy is the right trade against holding a lock longer.
const credCacheMaxEntries = 512

// credCacheKey identifies one cached credential.
//
// The key is (project, provider) and NOT (project, provider, connection-id) because a
// project cannot have more than one live connection for a provider: migration 000001
// creates `uq_<provider>_connections_project ON <table> (project_id) WHERE status <>
// 'deleted'` for all seven provider tables, so the live row is unique by construction.
// Delete SOFT-deletes, which leaves tombstones outside that index and lets a project
// reconnect — the new row is a different version (and a different id) under the SAME key,
// which the version check below treats as a miss.
//
// A comparable struct, not a formatted string: two fields that could otherwise be
// concatenated into a colliding key ("a:b" + "c" vs "a" + "b:c") stay distinct, and the
// map hashes it directly.
type credCacheKey struct {
	projectID string
	provider  model.Provider
}

// credCacheEntry is one cached decrypted credential plus the row version it was decrypted
// from.
type credCacheEntry struct {
	res *resolved
	// connID is the connection row's primary key. It is checked ALONGSIDE version because
	// version alone does not distinguish a reconnect: Delete soft-deletes, and Create INSERTs a
	// fresh row whose version starts at the column DEFAULT of 1 (migration 000001). So a project
	// that connected, dispatched once (caching at version 1), disconnected and reconnected within
	// the TTL produces a DIFFERENT row, with a different credential, at the SAME key and the SAME
	// version 1 — and a version-only check would serve the old plaintext against the new account.
	// The id is unique per row, so it separates them.
	connID string
	// version is the connections row's optimistic-concurrency counter as it stood when
	// this entry's plaintext was decrypted. It is the whole invalidation mechanism: every
	// mutating statement in ConnectionRepo bumps it (`version = version + 1` in update,
	// SetCredential and Delete), so a credential that has been rotated, re-pointed or
	// revoked cannot match a version cached before the change.
	version int64
	// lastUsed drives both TTL expiry and overflow eviction. Updated on every hit so an
	// actively used credential is not evicted underneath a busy project.
	lastUsed time.Time
}

// credCache caches DECRYPTED credentials keyed by (project, provider), so a dispatch burst
// or a polling dashboard performs one decrypt instead of one per call.
//
// # What it deliberately does not cache
//
// It does not cache the connection ROW. Every resolve still issues the repository Get —
// a single-row lookup on a unique index, which is not the cost this change targets — and
// the row it returns is what the entry is validated against. The expensive work is the
// AES-256-GCM decrypt and, downstream, the OAuth token exchange that a rebuilt platform
// client would have to redo because each client owns its own token cache; both are what
// reuse actually saves.
//
// # Why that choice settles invalidation and multi-replica together
//
// Caching the row instead would have required an eviction hook on every mutation path,
// and an in-process hook cannot evict on the OTHER replicas — the pod that serves the
// next request is not necessarily the pod that handled the write, so a revoked credential
// would keep being served elsewhere until a TTL expired. Validating against a version
// read fresh on every call removes that failure mode rather than bounding it: the write
// lands in Postgres, which every replica reads, so the first resolve after a rotation
// misses on EVERY replica at once. There is no staleness window to defend and no shared
// cache store to operate.
//
// The cost is one indexed SELECT per resolve, which the service already paid before this
// change. That is the deliberate trade: this is a decrypt/token cache, not a database
// cache, because a credential cache that can serve a revoked credential is a security
// defect and no hit rate is worth it.
//
// # Concurrency
//
// mu guards the map. Decrypts are coalesced through a singleflight group keyed by the
// same (project, provider) identity plus the row id and version the entry is validated on,
// so N concurrent callers that miss perform ONE decrypt rather than N. The lock is never held
// across a decrypt.
//
// That is a decrypt guarantee ONLY. Each caller receives its own clone and builds its own
// platform client, and the OAuth token is cached on the client instance — so this coalescing
// does not by itself collapse the token exchange. Reusing the CLIENT is what does that, and it
// is a separate cache (clientCache). Which dispatchers are wired to it is recorded on
// clientCache itself rather than repeated here, so the roster has one home to update.
type credCache struct {
	mu      sync.Mutex
	entries map[credCacheKey]*credCacheEntry
	group   singleflight.Group

	// now is the clock, injectable so tests can exercise TTL expiry without sleeping.
	now func() time.Time
	// ttl and maxEntries are per-instance so tests can pin small values.
	ttl        time.Duration
	maxEntries int
}

// newCredCache returns a cache with the production TTL and size bound.
func newCredCache() *credCache {
	return &credCache{
		entries:    make(map[credCacheKey]*credCacheEntry),
		now:        time.Now,
		ttl:        credCacheTTL,
		maxEntries: credCacheMaxEntries,
	}
}

// get returns the cached credential for key when one is present, unexpired, and decrypted
// from exactly the version supplied by the caller's fresh row read.
//
// The version argument is why this is safe: the caller has already read the current row,
// so a mismatch means the credential changed and the entry is dropped rather than served.
func (c *credCache) get(key credCacheKey, connID string, version int64) (*resolved, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	// A rotated/revoked credential fails HERE. Drop the entry rather than leaving it to
	// expire: its plaintext is now known to be superseded, so there is no reason to keep
	// it resident, and no later call can be served from it.
	if entry.version != version || entry.connID != connID {
		delete(c.entries, key)
		return nil, false
	}
	if now := c.now(); now.Sub(entry.lastUsed) >= c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	entry.lastUsed = c.now()
	return entry.res, true
}

// put stores the decrypted credential for key, recording the version it was decrypted
// from.
func (c *credCache) put(key credCacheKey, connID string, version int64, res *resolved) {
	if res == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked()
	if len(c.entries) >= c.maxEntries {
		if _, replacing := c.entries[key]; !replacing {
			c.evictOldestLocked()
		}
	}
	c.entries[key] = &credCacheEntry{res: res, connID: connID, version: version, lastUsed: c.now()}
}

// evictExpiredLocked drops every entry past its TTL. Called on write, which is the only
// path that needs the room; a read drops its own expired entry inline. Callers hold mu.
func (c *credCache) evictExpiredLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.lastUsed) >= c.ttl {
			delete(c.entries, k)
		}
	}
}

// evictOldestLocked removes the least-recently-used entry. Callers hold mu.
func (c *credCache) evictOldestLocked() {
	var (
		oldestKey credCacheKey
		oldest    time.Time
		found     bool
	)
	for k, e := range c.entries {
		if !found || e.lastUsed.Before(oldest) {
			oldestKey, oldest, found = k, e.lastUsed, true
		}
	}
	if found {
		delete(c.entries, oldestKey)
	}
}

// buildOnce returns the cached client for this identity, calling build at most once per
// concurrent burst when there is none.
//
// The double check inside the flight is deliberate: the leader of a burst that arrives just after
// another burst populated the entry must not rebuild it.
func (c *clientCache) buildOnce(key credCacheKey, connID string, version int64, build func() (any, error)) (any, error) {
	if cached, ok := c.get(key, connID, version); ok {
		return cached, nil
	}
	flightKey := key.projectID + "\x00" + string(key.provider) + "\x00" + connID + "\x00" + strconv.FormatInt(version, 10)
	v, err, _ := c.group.Do(flightKey, func() (any, error) {
		if cached, ok := c.get(key, connID, version); ok {
			return cached, nil
		}
		client, berr := build()
		if berr != nil {
			return nil, berr
		}
		c.put(key, connID, version, client)
		return client, nil
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// len reports the number of live entries. Test-facing.
func (c *credCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// decryptOnce runs fn at most once per concurrent burst for key, so N simultaneous callers
// that all miss the cache perform ONE decrypt (and, downstream, one token exchange)
// instead of N.
//
// The singleflight key is the same (project, provider) identity the cache uses, with the row
// id and version appended: two bursts either side of a credential rotation — or either side of a
// disconnect/reconnect — are different work and must not share a result. Without them a caller
// arriving just after the change could be handed the previous leader's plaintext.
func (c *credCache) decryptOnce(key credCacheKey, connID string, version int64, fn func() (*resolved, error)) (*resolved, error) {
	flightKey := key.projectID + "\x00" + string(key.provider) + "\x00" + connID + "\x00" + strconv.FormatInt(version, 10)
	v, err, _ := c.group.Do(flightKey, func() (any, error) {
		// Re-check the cache INSIDE the flight. A caller that missed get() can be descheduled
		// and arrive here after an earlier flight has already completed and populated the
		// entry; singleflight only coalesces callers whose Do calls OVERLAP, so that
		// straggler would otherwise start a fresh flight and decrypt a credential that is
		// already in the map. Measured: five concurrent resolves cost TWO decrypts without
		// this check, and a caller entering Do after a completed flight cost a second decrypt
		// every time. The re-check is under the same (id, version) validation as any other
		// read, so it can only return a credential this caller's own fresh row read agrees
		// with — a straggler holding a SUPERSEDED version still misses and decrypts.
		if cached, ok := c.get(key, connID, version); ok {
			return cached, nil
		}
		return fn()
	})
	if err != nil {
		return nil, err
	}
	res, _ := v.(*resolved)
	return res, nil
}

// cacheKeyFor builds the cache key for a resolution.
func cacheKeyFor(projectID string, provider model.Provider) credCacheKey {
	return credCacheKey{projectID: projectID, provider: provider}
}

// credCacheRegistry hands every credsSource built over the SAME (repo, encryptor) pair the SAME
// cache.
//
// It exists because the eight adapters are constructed independently — NewGoogleAdsDispatcher,
// NewRedditDispatcher, … NewAudienceBuilder each call newCredsSource — so allocating a cache per
// credsSource would produce eight private caches, not the shared resolver cache this is. Three
// things go wrong with eight: the reuse never crosses adapters (HubSpot is resolved by BOTH
// HubSpotDispatcher and AudienceBuilder, and each would decrypt separately), the plaintext
// residency bound silently becomes 8 × credCacheMaxEntries, and the provider component of the key
// becomes near-dead because each cache is already per-adapter.
//
// The pair is the correct identity, not a package-level singleton: the cache holds credentials
// DECRYPTED by a specific encryptor out of rows read through a specific repository. Two containers
// in one process (as tests build) must not share entries, because the same (project, provider) key
// means a different credential under a different key or a different database. Keying on the pair
// makes sharing exactly as wide as the credential domain and no wider.
//
// Entries are keyed by interface VALUE, so this is identity for pointer-backed implementations —
// which both are in production (*postgres.ConnectionRepo, *crypto.AESGCM). A repo or encryptor that
// is a non-comparable type would panic on map insert, so the constructor falls back to a private
// cache for those rather than risking it.
type credCacheRegistryKey struct {
	repo any
	enc  any
}

var (
	credCacheRegistryMu sync.Mutex
	credCacheRegistry   = map[credCacheRegistryKey]*credCache{}
)

// sharedCredCache returns the cache for this (repo, encryptor) pair, creating it on first use.
//
// A pair whose components are not comparable (so unusable as a map key) gets its OWN cache instead
// of panicking: correctness never depends on sharing — sharing is the optimisation — so degrading
// to a private cache is the safe failure.
func sharedCredCache(repo connReader, enc domain.Encryptor) *credCache {
	if !isComparable(repo) || !isComparable(enc) {
		return newCredCache()
	}
	key := credCacheRegistryKey{repo: repo, enc: enc}
	credCacheRegistryMu.Lock()
	defer credCacheRegistryMu.Unlock()
	if c, ok := credCacheRegistry[key]; ok {
		return c
	}
	c := newCredCache()
	credCacheRegistry[key] = c
	return c
}

// isComparable reports whether v can be used as a map key without panicking.
//
// Both checks are required, and the second is the one that actually delivers the promise in this
// doc comment. Comparability of an INTERFACE value is decided by its dynamic value, not only its
// type: `struct{ tag any }` holding a slice is a comparable TYPE that panics with "hash of
// unhashable type" the moment it is used as a key. A type-only check therefore passes and defers
// the panic to the map insert — on a dispatch path, at the first resolve, rather than at
// construction. reflect.Value.Comparable answers the value-level question.
func isComparable(v any) bool {
	if v == nil {
		return false
	}
	if !reflect.TypeOf(v).Comparable() {
		return false
	}
	return reflect.ValueOf(v).Comparable()
}

// clientCache caches built platform CLIENTS, keyed and validated exactly like credCache.
//
// It exists because caching the decrypted credential is only half the cost. Each platform client
// owns its OWN OAuth token cache (see internal/platform/googleads/client.go's tokenMu/accessToken),
// so rebuilding the client per call re-mints the access token even when the credential came back
// from credCache untouched — measured at five token-endpoint hits across five resolves. Reusing
// the client is what makes a polling dashboard perform ONE token exchange instead of one per read,
// which is the reuse LFXV2-3036 asks for.
//
// THE ROSTER LIVES HERE. Wired today into GoogleAdsDispatcher, RedditDispatcher,
// MicrosoftDispatcher and — as of LFXV2-3033 — MetaDispatcher and LinkedInDispatcher, on their
// TOGGLE and METRICS paths (see the per-PATH bypass list below for which paths are excluded and
// why). The earlier deferral of Meta and LinkedIn was procedural, not technical: open PRs owned
// those files at the time (cs#148, cs#152, cs#158). Those PRs have since merged, so the stated
// reason is gone and both have been wired after their own safety analysis, recorded on the
// `clients` field in meta.go and linkedin.go.
//
// X/Twitter remains deliberately NOT wired, and its reason is TECHNICAL and still stands. The X
// client documents itself as safe for SEQUENTIAL use only, and that is not a stale doc comment:
// it paces its own writes with an inter-request sleep (twitter.Client.pace/writeDelay) to stay
// under X's ~1 write-per-second limit, a scheme that assumes the instance is driving one dispatch
// at a time. Sharing one across concurrent callers would interleave two dispatches through that
// single pacing assumption and break it — on the money-spending create path. Wiring it needs a
// concurrency argument this pattern does not supply, not merely a copy of the Meta/LinkedIn
// analysis.
//
// "Rebuilds a client" is NOT the same as "re-mints a token", and that distinction survives the
// wiring: Meta and LinkedIn are handed an already-minted bearer token and perform no exchange at
// construction, and X signs each request with stored OAuth 1.0a credentials. So the win for Meta
// and LinkedIn is allocation rather than a saved token round-trip, which is why their wiring
// tests assert client IDENTITY instead of counting hits on a token endpoint.
// Other comments point AT this list rather than restating it, so wiring the next provider is a
// one-site edit.
//
// WIRED IS NOT THE SAME AS "every path on a wired provider". The bypasses below are per-PATH, and
// this roster is the single source of truth for them, so enumerate them all:
//
//   - Google Ads' account-agnostic discovery client (empty CustomerID, resolveGoogleAdsDiscoveryClient),
//     Microsoft's ListAccounts client (ZERO AccountConfig) and Meta's discovery client
//     (resolveMetaDiscoveryClient, also a ZERO AccountConfig) — see cachedMicrosoftClient for why
//     sharing one key with dispatch would cross the two. The rule is general: a discovery client
//     names no account precisely so the answer is not narrowed, so it is a DIFFERENT object under
//     the same connection identity.
//   - **MetaDispatcher.Dispatch** and **LinkedInDispatcher.Dispatch**, for the same reason Google
//     Ads' create path is excluded but arrived at deliberately rather than by omission. Meta's
//     create client carries a fuller AccountConfig (page id, plus the account a create requires),
//     and LinkedIn's carries a RuntimeConfig with DefaultOrgID and the per-request
//     TargetingProfiles/EmployerExclusions read from the campaign config — so its client VARIES
//     per call under a cache key that does not. Caching either would let one campaign's targeting,
//     or one path's account config, serve another's. Neither re-mints a token at construction, so
//     unlike Google Ads' create path the cost of the exclusion is allocation only.
//   - Google Ads' adoption owned-connection path (resolveOwnedGoogleAdsClient → LookupCampaign): a
//     rare one-shot rather than the polling loop this exists for.
//   - **GoogleAdsDispatcher.Dispatch**, which builds its client inline via googleads.NewClient
//     rather than through cachedGoogleAdsClient. This one is easy to miss and was omitted here for
//     two tickets: Google Ads is listed as "wired", and it IS — but only on the toggle/metrics
//     entry point (resolveGoogleAdsClient → ToggleStatus, ReadMetrics). The CREATE path re-mints
//     an OAuth token per campaign, so a dispatch burst gets no reuse at all. Microsoft's create
//     path is the opposite (microsoft.go, Dispatch → cachedMicrosoftClient), which is exactly the
//     asymmetry that made this omission plausible. Both behaviours are pinned:
//     TestClientCache_GoogleAdsDispatchBypassesTheCache and
//     TestClientCache_MicrosoftDispatchUsesTheCachedClient — wiring Google Ads' create path will
//     fail the first, which is the prompt to update this list in the same change.
//
// It is a separate type from credCache rather than a generic because the two hold different things
// with different safety arguments: a *resolved is copied per caller (fromSystem is stamped on it),
// while a client is deliberately SHARED — sharing is the entire point, since the token cache lives
// on the instance.
//
// Sharing is safe only because of a property every stored client must have INDIVIDUALLY, and the
// field is typed `any`, so the compiler enforces none of it: each client guards its token cache
// and in-flight refresh handle with a mutex and stashes NO per-call state on the receiver. That
// was verified per provider rather than inherited from Google Ads — see the per-dispatcher
// comments on the `clients` field in googleads.go, reddit.go and microsoft.go, of which
// Microsoft's is the one that most needed checking (multi-customer discovery: a CustomerID that
// VARIED per caller would have made a shared instance serve one caller against another's
// customer). Its configured customer IS stashed on the receiver — c.account.CustomerID, which
// doCustomerRequest reads rather than taking as an argument — so what makes it safe is that
// c.account is immutable and the cache key pins the connection row id and version; the
// per-customer discovery path uses a separate ZERO-AccountConfig client that bypasses this cache.
// A future provider whose client stashes MUTABLE per-call state must NOT be wired to this cache
// without changing the client first.
//
// Entries carry the same (row id, version) validation as credCache, so a rotated or revoked
// credential can never be served through a stale client either — a client built from an old
// credential is exactly as dangerous as the old credential.
type clientCache struct {
	mu      sync.Mutex
	entries map[credCacheKey]*clientCacheEntry
	// group coalesces concurrent construction for the same identity. Without it a COLD key
	// behaves like no cache at all under a burst: N callers all miss, each builds its own
	// client, and each mints its own token from its own instance cache -- measured at 16 token
	// exchanges across 16 concurrent callers. Warm-key reuse alone does not cover this, and a
	// sequential test cannot see it.
	group singleflight.Group

	now        func() time.Time
	ttl        time.Duration
	maxEntries int
}

type clientCacheEntry struct {
	client   any
	connID   string
	version  int64
	lastUsed time.Time
}

func newClientCache() *clientCache {
	return &clientCache{
		entries:    make(map[credCacheKey]*clientCacheEntry),
		now:        time.Now,
		ttl:        credCacheTTL,
		maxEntries: credCacheMaxEntries,
	}
}

// get returns the cached client when it was built from exactly this row id and version.
func (c *clientCache) get(key credCacheKey, connID string, version int64) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if entry.version != version || entry.connID != connID {
		delete(c.entries, key)
		return nil, false
	}
	if now := c.now(); now.Sub(entry.lastUsed) >= c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	entry.lastUsed = c.now()
	return entry.client, true
}

// put stores a built client under the row identity it was built from.
func (c *clientCache) put(key credCacheKey, connID string, version int64, client any) {
	if client == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.lastUsed) >= c.ttl {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= c.maxEntries {
		if _, replacing := c.entries[key]; !replacing {
			var (
				oldestKey credCacheKey
				oldest    time.Time
				found     bool
			)
			for k, e := range c.entries {
				if !found || e.lastUsed.Before(oldest) {
					oldestKey, oldest, found = k, e.lastUsed, true
				}
			}
			if found {
				delete(c.entries, oldestKey)
			}
		}
	}
	c.entries[key] = &clientCacheEntry{client: client, connID: connID, version: version, lastUsed: now}
}
