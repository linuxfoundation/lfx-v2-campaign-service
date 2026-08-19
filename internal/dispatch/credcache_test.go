// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// countingEncryptor records how many times Decrypt ran and returns the ciphertext
// unchanged. The count is the whole point: it is the observable that distinguishes a cache
// hit from a miss, and — because each platform client mints its own OAuth token from the
// credential it is built with — one decrypt is also one token exchange downstream.
type countingEncryptor struct{ decrypts atomic.Int64 }

func (e *countingEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }

func (e *countingEncryptor) Decrypt(c []byte) ([]byte, error) {
	e.decrypts.Add(1)
	return c, nil
}

// versionedConn is usableConn plus an explicit row version, which is what the cache
// validates every hit against.
func versionedConn(creds, accountID string, version int64) *model.Connection {
	return idConn("conn-"+accountID, creds, accountID, version)
}

// idConn builds a connection row with an explicit primary key, version, credential blob and
// account. The ID matters because the cache checks it alongside the version — see
// TestCredCache_ReconnectAtSameVersionIsNotServedFromCache.
func idConn(id, creds, accountID string, version int64) *model.Connection {
	c := usableConn(creds, accountID)
	c.ID = id
	c.Version = version
	return c
}

// TestCredCache_ReusesDecryptedCredential pins the reuse this change exists for: repeated
// resolves for the same project+provider decrypt ONCE.
//
// Before this change every dispatch, toggle and metrics read re-decrypted, so a dashboard
// polling metrics performed a credential decrypt (and a fresh OAuth exchange) per read.
func TestCredCache_ReusesDecryptedCredential(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf": versionedConn(`{"a":1}`, "acct-1", 7),
	}}
	enc := &countingEncryptor{}
	src := newCredsSource(repo, enc)

	for i := range 5 {
		got, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		if string(got.plaintext) != `{"a":1}` || got.accountID != "acct-1" {
			t.Fatalf("resolve #%d returned %q/%q, want the stored credential", i, got.plaintext, got.accountID)
		}
	}
	if n := enc.decrypts.Load(); n != 1 {
		t.Errorf("decrypts = %d, want 1 — the credential must be decrypted once and reused", n)
	}
}

// TestCredCache_RotatedCredentialIsNotServedFromCache is the load-bearing test for this
// change.
//
// It does NOT assert that an eviction hook fired; it asserts the OUTCOME that matters — a
// credential rotated (or revoked) after being cached is never handed to a caller again.
// Asserting a call count would pass against a broken implementation that evicted the wrong
// key or evicted and then re-populated from a stale read, so the assertion is on the
// PLAINTEXT the second resolve receives.
func TestCredCache_RotatedCredentialIsNotServedFromCache(t *testing.T) {
	row := versionedConn(`{"secret":"old"}`, "acct-old", 3)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"cncf": row}}
	enc := &countingEncryptor{}
	src := newCredsSource(repo, enc)

	first, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if string(first.plaintext) != `{"secret":"old"}` {
		t.Fatalf("first resolve = %q, want the original credential", first.plaintext)
	}

	// Rotate the credential exactly as ConnectionRepo does: replace the blob and bump the
	// version (`version = version + 1` in SetCredential/update). The cache entry for this
	// key is now stale, and the ONLY thing standing between the caller and the superseded
	// plaintext is the version check.
	repo.rows["cncf"] = versionedConn(`{"secret":"new"}`, "acct-new", 4)

	second, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if string(second.plaintext) == `{"secret":"old"}` {
		t.Fatal("resolve served the PRE-ROTATION credential from cache — a revoked credential " +
			"would still authenticate; this is the failure this cache must not introduce")
	}
	if string(second.plaintext) != `{"secret":"new"}` {
		t.Errorf("resolve = %q, want the rotated credential %q", second.plaintext, `{"secret":"new"}`)
	}
	if second.accountID != "acct-new" {
		t.Errorf("accountID = %q, want the rotated row's account %q", second.accountID, "acct-new")
	}
	if n := enc.decrypts.Load(); n != 2 {
		t.Errorf("decrypts = %d, want 2 (one per distinct credential version)", n)
	}
}

// TestCredCache_RevokedConnectionIsNotServedFromCache covers the revocation shape the
// rotation test does not: the connection is DELETED rather than re-credentialed.
//
// Delete soft-deletes and Get filters the row out, so the next resolve must fail rather
// than fall through to a cached plaintext. A cache consulted BEFORE the repository read
// would happily serve the deleted project's credential; this pins that it is consulted
// after.
func TestCredCache_RevokedConnectionIsNotServedFromCache(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf": versionedConn(`{"secret":"live"}`, "acct-1", 2),
	}}
	src := newCredsSource(repo, &countingEncryptor{})

	if _, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Revoke: the row is gone from Get's view, and there is no system row to fall back to.
	delete(repo.rows, "cncf")

	got, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err == nil {
		t.Fatalf("resolve returned credentials %q for a revoked connection", got.plaintext)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a connection that no longer exists", err)
	}
}

// TestCredCache_ConcurrentResolvesShareOneDecrypt pins the concurrency property: N
// simultaneous callers for the same key perform ONE decrypt — and therefore, because every
// caller builds its platform client from that single decrypted credential, ONE token
// exchange rather than N.
//
// The assertion is on the exchange (decrypt) COUNT, not merely that each caller got a
// credential back: an implementation with no coalescing returns N valid credentials and
// would pass a "did I get a client?" check while performing exactly the N-fold work this
// change exists to remove.
func TestCredCache_ConcurrentResolvesShareOneDecrypt(t *testing.T) {
	// A decrypt that blocks until released, so every goroutine is guaranteed to be inside
	// the miss window at once. Without this the first caller could finish and populate the
	// cache before the others start, and the test would pass without proving coalescing.
	const callers = 32
	release := make(chan struct{})
	enc := &blockingEncryptor{release: release}
	// A concurrency-safe reader. scopedConnReader records every Get in an unguarded slice —
	// correct for the serial tests that own it, but its own data race under -race here, and
	// it would mask the property this test is actually about.
	repo := &syncConnReader{row: versionedConn(`{"a":1}`, "acct-1", 1), barrier: callers}
	src := newCredsSource(repo, enc)

	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
			errMu.Lock()
			defer errMu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case string(got.plaintext) != `{"a":1}`:
				errs = append(errs, fmt.Errorf("plaintext = %q", got.plaintext))
			}
		}()
	}

	// Hold the decrypt open until every caller has had the chance to join the flight, then
	// release it.
	//
	// Getting this barrier right took three attempts, and the failures are worth recording
	// because each looked correct. (1) Waiting for the FIRST decrypt to begin lets the leader
	// finish and populate the cache while stragglers sit between Get and decryptOnce, so each
	// straggler leads its own flight — measured at 3 failures in 12 runs, and independently
	// reproduced at 11/40. (2) Signalling before calling resolve proves only that a goroutine
	// was scheduled, not that it reached the flight — measured decrypts = 17. (3) A rendezvous
	// inside repo.Get releases all callers at the same instant but still does not order them
	// against decryptOnce, so the leader can complete first — measured decrypts = 18.
	//
	// The property being asserted is that concurrent callers COALESCE, and the only sound way
	// to hold them together is to keep the leader's decrypt in flight until every other caller
	// has demonstrably entered the singleflight. blockingEncryptor keeps the leader inside
	// Decrypt for the whole window; waitForN reports when the group's waiter count shows the
	// followers have joined it.
	repo.waitForAll()
	waitForAllInFlight(t, callers)
	close(release)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent resolve: %v", err)
	}
	if n := enc.decrypts.Load(); n != 1 {
		t.Errorf("decrypts = %d across %d concurrent callers, want 1 — each decrypt is a "+
			"separate OAuth token exchange downstream", n, callers)
	}
}

// syncConnReader is a minimal connReader safe for concurrent Get, returning one row for
// every project. It exists because the shared scopedConnReader fake records its calls in an
// unsynchronised slice.
//
// When barrier is non-nil, Get blocks every caller until `barrier` of them have arrived — an
// n-party rendezvous that puts all callers inside the cache-miss window simultaneously, which is
// the precondition the coalescing assertion rests on and which no sleep can guarantee.
type syncConnReader struct {
	row *model.Connection

	barrier  int
	mu       sync.Mutex
	arrived  int
	allHere  chan struct{}
	initOnce sync.Once
}

func (r *syncConnReader) allHereCh() chan struct{} {
	r.initOnce.Do(func() { r.allHere = make(chan struct{}) })
	return r.allHere
}

// waitForAll blocks until `barrier` callers have entered Get.
func (r *syncConnReader) waitForAll() { <-r.allHereCh() }

func (r *syncConnReader) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	if r.barrier > 0 {
		ch := r.allHereCh()
		r.mu.Lock()
		r.arrived++
		reached := r.arrived == r.barrier
		r.mu.Unlock()
		if reached {
			close(ch)
		}
		<-ch
	}
	return r.row, nil
}

func (r *syncConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// blockingEncryptor holds every Decrypt until release is closed, so the leader's decrypt stays
// in flight for as long as the test needs the followers to pile onto it.
type blockingEncryptor struct {
	decrypts atomic.Int64
	release  chan struct{}
}

func (e *blockingEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }

func (e *blockingEncryptor) Decrypt(c []byte) ([]byte, error) {
	e.decrypts.Add(1)
	<-e.release
	return c, nil
}

// TestCredCache_SystemFallbackDoesNotShareProjectEntry pins the scoping property: a project
// resolving through the LF system fallback caches under the reserved system scope, so it
// can neither read nor poison a project-owned entry.
//
// The risk if the key were the CALLING project rather than the resolved scope: two projects
// on the fallback would each cache the LF credential under their own name, multiplying the
// resident copies of the single most sensitive credential in the system, and a project that
// later connects its own account could be served the LF one.
func TestCredCache_SystemFallbackDoesNotShareProjectEntry(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: versionedConn(`{"sys":true}`, "sys-account", 1),
	}}
	enc := &countingEncryptor{}
	src := newCredsSource(repo, enc)

	// Two DIFFERENT projects, both with no connection of their own, both falling back.
	for _, project := range []string{"cncf", "lfai"} {
		got, err := src.resolve(context.Background(), project, model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve(%s): %v", project, err)
		}
		if !got.fromSystem {
			t.Errorf("resolve(%s): fromSystem = false, want true on the fallback path", project)
		}
		if got.accountID != "sys-account" {
			t.Errorf("resolve(%s): accountID = %q, want the system account", project, got.accountID)
		}
	}
	// One entry, under the system scope — reached by both projects.
	if n := enc.decrypts.Load(); n != 1 {
		t.Errorf("decrypts = %d, want 1: both projects resolve the SAME system row, which is "+
			"cached once under the reserved scope", n)
	}
	if _, ok := src.cache.get(cacheKeyFor(model.SystemProjectID, model.ProviderGoogleAds), "conn-sys-account", 1); !ok {
		t.Error("the fallback credential is not cached under the system scope")
	}
	for _, project := range []string{"cncf", "lfai"} {
		if _, ok := src.cache.get(cacheKeyFor(project, model.ProviderGoogleAds), "conn-sys-account", 1); ok {
			t.Errorf("the LF system credential is cached under project scope %q; it must be "+
				"keyed by the scope it was resolved from", project)
		}
	}
}

// TestCredCache_ProvidersDoNotShareAnEntry: the key is (project, provider), so one
// project's Google credential must never satisfy a lookup for its Reddit credential.
func TestCredCache_ProvidersDoNotShareAnEntry(t *testing.T) {
	c := newCredCache()
	ga := cacheKeyFor("cncf", model.ProviderGoogleAds)
	rd := cacheKeyFor("cncf", model.ProviderRedditAds)
	c.put(ga, "id-ga", 1, &resolved{plaintext: []byte(`{"google":true}`)})

	if _, ok := c.get(rd, "id-ga", 1); ok {
		t.Error("a reddit lookup was satisfied by the google entry — the provider is part of the key")
	}
	got, ok := c.get(ga, "id-ga", 1)
	if !ok || string(got.plaintext) != `{"google":true}` {
		t.Errorf("google lookup = %v/%q, want the stored google credential", ok, got.plaintext)
	}
}

// TestCredCache_TTLExpires: an entry past the TTL is dropped rather than served, bounding
// how long decrypted credentials stay resident for a project that has gone quiet.
func TestCredCache_TTLExpires(t *testing.T) {
	now := time.Now()
	c := newCredCache()
	c.now = func() time.Time { return now }
	c.ttl = time.Minute

	key := cacheKeyFor("cncf", model.ProviderGoogleAds)
	c.put(key, "id-1", 1, &resolved{plaintext: []byte(`{"a":1}`)})
	if _, ok := c.get(key, "id-1", 1); !ok {
		t.Fatal("entry missing immediately after put")
	}

	now = now.Add(time.Minute) // exactly at the TTL — the boundary is inclusive of expiry
	if _, ok := c.get(key, "id-1", 1); ok {
		t.Error("an entry at its TTL was served; decrypted credentials must not outlive the bound")
	}
	if n := c.len(); n != 0 {
		t.Errorf("entries = %d after expiry, want 0 — the plaintext must not stay resident", n)
	}
}

// TestCredCache_EvictsOldestOnOverflow: the cache is bounded, so decrypted credentials
// cannot accumulate indefinitely as projects are added.
func TestCredCache_EvictsOldestOnOverflow(t *testing.T) {
	now := time.Now()
	c := newCredCache()
	c.now = func() time.Time { return now }
	c.maxEntries = 2

	first := cacheKeyFor("p1", model.ProviderGoogleAds)
	c.put(first, "id-p1", 1, &resolved{plaintext: []byte(`1`)})
	now = now.Add(time.Second)
	c.put(cacheKeyFor("p2", model.ProviderGoogleAds), "id-p2", 1, &resolved{plaintext: []byte(`2`)})
	now = now.Add(time.Second)
	c.put(cacheKeyFor("p3", model.ProviderGoogleAds), "id-p3", 1, &resolved{plaintext: []byte(`3`)})

	if n := c.len(); n != 2 {
		t.Errorf("entries = %d, want the cache bounded at 2", n)
	}
	if _, ok := c.get(first, "id-p1", 1); ok {
		t.Error("the least-recently-used entry survived overflow eviction")
	}
}

// TestCredCache_ConcurrentDistinctKeysAreRaceFree exercises the map under -race with many
// goroutines hitting DIFFERENT keys, including eviction pressure. It asserts no corruption
// (the -race detector and the bound are the assertions).
func TestCredCache_ConcurrentDistinctKeysAreRaceFree(t *testing.T) {
	c := newCredCache()
	c.maxEntries = 8

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := cacheKeyFor(fmt.Sprintf("p%d", i), model.ProviderGoogleAds)
			c.put(key, "id", 1, &resolved{plaintext: []byte(fmt.Sprintf("%d", i))})
			c.get(key, "id", 1)
			c.len()
		}()
	}
	wg.Wait()

	if n := c.len(); n > c.maxEntries {
		t.Errorf("entries = %d, want the bound of %d respected under concurrency", n, c.maxEntries)
	}
}

// TestCredCache_SystemScopeResolvedDirectlyIsNotTaggedFromSystem is the test that actually
// pins the per-call copy, and it exists because a weaker one did not.
//
// The FALLBACK path stamps fromSystem on the value it returns. If callers share the cached
// struct, that stamp is written INTO the cache entry — and the next caller to resolve the
// system scope DIRECTLY (model.SystemProjectID is an ordinary project id to resolveOwned,
// and the system account's own campaigns dispatch through it) reads back a value claiming
// it came from the fallback. It did not: it is that project's own connection.
//
// Downstream, fromSystem decides whether an unusable-connection defect is reported as the
// caller's 400 or as a 500 that pages whoever installed the LF credential — so a stale true
// misroutes a real defect. An earlier version of this file checked a project-owned resolve
// on a DIFFERENT key, which cannot observe the leak at all: distinct keys never share an
// entry, so that test passed with the copy removed.
func TestCredCache_SystemScopeResolvedDirectlyIsNotTaggedFromSystem(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: versionedConn(`{"sys":true}`, "sys-account", 1),
	}}
	src := newCredsSource(repo, &countingEncryptor{})

	// A project with no connection of its own falls back and is tagged.
	fallback, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("fallback resolve: %v", err)
	}
	if !fallback.fromSystem {
		t.Fatal("fallback resolve: fromSystem = false, want true")
	}

	// Now resolve the system scope AS ITS OWN PROJECT. This hits the very entry the stamp
	// above would have polluted, via a path that never sets the flag itself.
	direct, err := src.resolveOwned(context.Background(), model.SystemProjectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("direct resolve of the system scope: %v", err)
	}
	if direct.fromSystem {
		t.Error("a connection resolved as the project's OWN is tagged fromSystem — the " +
			"fallback's stamp was written into the shared cache entry, so a defect on this " +
			"row would be reported against the LF fallback instead of its real owner")
	}
}

// TestCredCache_SingleflightDoesNotShareAcrossVersions pins that the decrypt-coalescing key
// includes the row version, so a burst that straddles a credential rotation is not served
// the pre-rotation plaintext.
//
// This is the rotation guarantee under CONCURRENCY, and it is a distinct mechanism from the
// cache's version check: the cache is never consulted for an in-flight decrypt, so a
// version-independent singleflight key would let a caller that read the NEW row join the
// leader's OLD flight and receive the superseded credential — precisely the "revoked
// credential still authenticates" outcome, just through the coalescing path.
//
// The test drives it deterministically: the leader blocks inside Decrypt holding the old
// blob; the row is rotated while it blocks; a second caller then resolves and must NOT get
// the leader's result.
func TestCredCache_SingleflightDoesNotShareAcrossVersions(t *testing.T) {
	oldRow := versionedConn(`{"secret":"old"}`, "acct-old", 1)
	newRow := versionedConn(`{"secret":"new"}`, "acct-new", 2)

	repo := &swappableConnReader{}
	repo.row.Store(oldRow)

	enteredDecrypt := make(chan struct{})
	releaseLeader := make(chan struct{})
	enc := &gatedEncryptor{entered: enteredDecrypt, release: releaseLeader, gateOn: []byte(`{"secret":"old"}`)}
	src := newCredsSource(repo, enc)

	leaderDone := make(chan *resolved, 1)
	go func() {
		got, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			leaderDone <- nil
			return
		}
		leaderDone <- got
	}()

	<-enteredDecrypt       // the leader is inside the decrypt, holding the OLD blob
	repo.row.Store(newRow) // rotate while it is in flight

	// A second caller now reads the NEW row (version 2) and must not be coalesced onto the
	// leader's version-1 flight.
	second := make(chan *resolved, 1)
	go func() {
		got, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			second <- nil
			return
		}
		second <- got
	}()

	// Bounded wait. A version-INDEPENDENT singleflight key does not merely return the wrong
	// credential here — it makes the second caller join the leader's flight and block until
	// the leader is released, which this test does not do until after it has read the
	// result. That is a hang, and a hang is not a test failure anyone can read, so the wait
	// is bounded and the timeout is reported as the finding it is.
	var got *resolved
	select {
	case got = <-second:
	case <-time.After(10 * time.Second):
		close(releaseLeader)
		<-leaderDone
		t.Fatal("the post-rotation resolve blocked on the in-flight decrypt for the OLD " +
			"version: the singleflight key does not include the version, so a rotation " +
			"cannot overtake a burst already in flight")
	}
	close(releaseLeader)
	<-leaderDone

	if got == nil {
		t.Fatal("the post-rotation resolve failed")
	}
	if string(got.plaintext) == `{"secret":"old"}` {
		t.Fatal("a caller that read the ROTATED row was served the pre-rotation credential " +
			"by joining an in-flight decrypt for the old version — the singleflight key must " +
			"include the version")
	}
	if string(got.plaintext) != `{"secret":"new"}` {
		t.Errorf("plaintext = %q, want the rotated credential", got.plaintext)
	}
}

// swappableConnReader returns whichever row is currently stored, safely across goroutines.
type swappableConnReader struct {
	row atomic.Pointer[model.Connection]
}

func (r *swappableConnReader) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	return r.row.Load(), nil
}

func (r *swappableConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// gatedEncryptor blocks the decrypt of one specific ciphertext (gateOn) until released,
// signalling once it has entered. Every other ciphertext decrypts immediately, so the
// second caller is never gated behind the leader at the ENCRYPTOR level — only the
// singleflight key can couple them, which is what the test is about.
type gatedEncryptor struct {
	entered   chan struct{}
	release   chan struct{}
	gateOn    []byte
	enterOnce sync.Once
}

func (e *gatedEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }

func (e *gatedEncryptor) Decrypt(c []byte) ([]byte, error) {
	if string(c) == string(e.gateOn) {
		e.enterOnce.Do(func() { close(e.entered) })
		<-e.release
	}
	return c, nil
}

// TestCredCache_CredentialsRemovedFromRowIsNotServedFromCache covers the ordering question
// the cache read introduces: it now runs BEFORE the "row has no stored credentials" check.
//
// That is safe only because nothing is ever cached without a successful decrypt, so a row
// whose blob was emptied cannot have a live entry under its CURRENT version. This pins the
// outcome rather than the reasoning: emptying the credential (and bumping the version, as
// every mutation does) must produce the unusable-connection error, not the previously
// cached plaintext.
func TestCredCache_CredentialsRemovedFromRowIsNotServedFromCache(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf": versionedConn(`{"secret":"live"}`, "acct-1", 1),
	}}
	src := newCredsSource(repo, &countingEncryptor{})

	if _, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	stripped := versionedConn("", "acct-1", 2)
	stripped.EncryptedCredentials = nil
	repo.rows["cncf"] = stripped

	got, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err == nil {
		t.Fatalf("resolve returned %q for a row with no stored credentials", got.plaintext)
	}
	if !errors.Is(err, domain.ErrCredentialsAbsent) {
		t.Errorf("err = %v, want ErrCredentialsAbsent", err)
	}
}

// TestCredCache_ReconnectAtSameVersionIsNotServedFromCache pins the one shape a version-only
// check cannot catch.
//
// Delete SOFT-deletes, and Create INSERTs a fresh row whose version starts at the column
// DEFAULT of 1 (migration 000001, `version BIGINT NOT NULL DEFAULT 1`). So a project that
// connects, dispatches once while still at version 1, disconnects and reconnects a DIFFERENT
// ad account produces a new row — new id, new credential — at the SAME cache key and the SAME
// version. A cache keyed on (project, provider) and validated on version alone serves the old
// plaintext against the new account, silently, for the rest of the TTL.
//
// The assertion is on the credential and the account the second resolve receives, not on a
// count: serving the OLD account's credential is the failure, and it is invisible to any
// count-based check.
func TestCredCache_ReconnectAtSameVersionIsNotServedFromCache(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf": idConn("conn-old", `{"secret":"old"}`, "acct-old", 1),
	}}
	enc := &countingEncryptor{}
	src := newCredsSource(repo, enc)

	first, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first.accountID != "acct-old" {
		t.Fatalf("first resolve account = %q, want acct-old", first.accountID)
	}

	// Disconnect + reconnect a different account. New row, new id, new credential — and the
	// version restarts at the INSERT default, colliding with what is cached.
	repo.rows["cncf"] = idConn("conn-new", `{"secret":"new"}`, "acct-new", 1)

	second, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second.accountID == "acct-old" || string(second.plaintext) == `{"secret":"old"}` {
		t.Fatal("resolve served the DISCONNECTED account's credential from cache: the reconnected " +
			"row restarts at version 1, so version alone cannot distinguish it from the row it " +
			"replaced — dispatches would run against the account the project just disconnected")
	}
	if second.accountID != "acct-new" || string(second.plaintext) != `{"secret":"new"}` {
		t.Errorf("resolve = %q/%q, want the reconnected account's credential",
			second.plaintext, second.accountID)
	}
}

// TestCredCache_IsSharedAcrossDispatchers pins that this is ONE resolver cache, not eight.
//
// The eight adapters are constructed independently (NewGoogleAdsDispatcher … NewAudienceBuilder),
// each calling newCredsSource. Allocating a cache per credsSource compiles, passes every
// single-adapter test, and quietly defeats the change: reuse never crosses adapters, the
// plaintext residency bound becomes 8 × credCacheMaxEntries, and the provider component of the
// key goes dead because each cache is already per-adapter.
//
// HubSpot is the case that proves it, because it is the one provider with TWO consumers in
// production — HubSpotDispatcher and AudienceBuilder both resolve it for the same project.
func TestCredCache_IsSharedAcrossDispatchers(t *testing.T) {
	repo := &syncConnReader{row: idConn("conn-hs", `{"hs":true}`, "portal-1", 1)}
	enc := &countingEncryptor{}

	// Two independently constructed credsSources over the SAME (repo, encryptor) pair, exactly
	// as the container builds two HubSpot consumers.
	a := newCredsSource(repo, enc)
	b := newCredsSource(repo, enc)

	if _, err := a.resolve(context.Background(), "cncf", model.ProviderHubSpot); err != nil {
		t.Fatalf("resolve via the first consumer: %v", err)
	}
	if _, err := b.resolve(context.Background(), "cncf", model.ProviderHubSpot); err != nil {
		t.Fatalf("resolve via the second consumer: %v", err)
	}

	if n := enc.decrypts.Load(); n != 1 {
		t.Errorf("decrypts = %d across two consumers of the same connection, want 1 — the cache "+
			"is per-credsSource rather than shared, so the reuse never crosses adapters and the "+
			"resident-plaintext bound is multiplied by the number of adapters", n)
	}
	if a.cache != b.cache {
		t.Error("the two credsSources hold different caches; they are built over the same repo " +
			"and encryptor and must share one")
	}
}

// TestCredCache_DistinctBackendsDoNotShareEntries is the other half of the sharing rule: the
// cache is shared as wide as the credential domain and NO wider.
//
// The same (project, provider) key means a DIFFERENT credential under a different encryptor or a
// different database. Two containers in one process (which tests build routinely) must therefore
// not see each other's entries — a package-level singleton would leak one test's plaintext into
// another's resolve, and in production would blur two deployments' credentials together.
func TestCredCache_DistinctBackendsDoNotShareEntries(t *testing.T) {
	row := idConn("conn-1", `{"secret":"A"}`, "acct-A", 1)

	repoA := &syncConnReader{row: row}
	repoB := &syncConnReader{row: idConn("conn-1", `{"secret":"B"}`, "acct-B", 1)}
	enc := &countingEncryptor{}

	a := newCredsSource(repoA, enc)
	b := newCredsSource(repoB, enc)

	if a.cache == b.cache {
		t.Fatal("two credsSources over DIFFERENT repositories share a cache; the same key means a " +
			"different credential in each, so entries must not cross")
	}

	gotA, err := a.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	gotB, err := b.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	if string(gotA.plaintext) != `{"secret":"A"}` || string(gotB.plaintext) != `{"secret":"B"}` {
		t.Errorf("A = %q, B = %q; each backend must serve its own credential",
			gotA.plaintext, gotB.plaintext)
	}
}

// waitForAllInFlight blocks until want goroutines are inside singleflight.Group.Do — the leader,
// which is parked in Decrypt beneath Do, PLUS every follower waiting on it.
//
// The leader is counted deliberately. Its stack contains Group.Do too (Do calls the closure, which
// blocks in Decrypt), so a threshold of want-1 can be satisfied by the leader plus want-2
// followers, leaving one caller stranded between its cache miss and Do — which after release
// starts a second flight and reintroduces the flake this barrier exists to remove. Waiting for all
// `callers` frames is what guarantees nobody is still in transit.
//
// It polls a property the test can actually observe — the number of goroutines blocked inside
// singleflight.Group.Do — rather than a proxy for it. Every proxy tried first (a decrypt has
// begun; all callers entered Get; all callers were scheduled) fails to order the followers
// against the leader's completion, which is precisely what the coalescing assertion needs. See
// the comment at the call site for the three measured failure modes.
//
// It reads the runtime's goroutine dump because singleflight exposes no waiter count. That is
// unusual in a test, and it is justified here by the alternative being a sleep — which cannot
// establish the happens-before at all and would reintroduce the flake.
func waitForAllInFlight(t *testing.T, want int) {
	t.Helper()
	if want <= 0 {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		// Followers park in Group.Do waiting on the shared call's WaitGroup.
		if strings.Count(string(buf[:n]), "singleflight.(*Group).Do") >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d callers were inside the in-flight decrypt within the deadline; the "+
				"test cannot prove coalescing without all of them", strings.Count(string(buf[:n]),
				"singleflight.(*Group).Do"), want)
		}
		runtime.Gosched()
	}
}

// TestClientCache_ReusedClientPerformsOneTokenExchange is the LFXV2-3036 test: it counts hits on
// the OAuth TOKEN ENDPOINT, not decrypts.
//
// The distinction is the whole ticket. Caching the decrypted credential removes the decrypt but
// NOT the token exchange, because each platform client caches its access token on the instance
// (internal/platform/googleads/client.go) — so a client rebuilt per resolve re-mints the token
// however cheap the credential lookup became. Measured before the client cache: five resolves
// produced five token-endpoint hits. A decrypt-count assertion cannot see that at all, which is
// why this test drives a real HTTP token endpoint and counts it.
func TestClientCache_ReusedClientPerformsOneTokenExchange(t *testing.T) {
	var tokenHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			tokenHits.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	row := idConn("conn-1", googleAdsTestCreds, "1234567890", 1)
	row.Provider = model.ProviderGoogleAds
	repo := &syncConnReader{row: row}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{},
		googleads.WithTokenURL(srv.URL+"/token"), googleads.WithBaseURL(srv.URL))

	// Five resolves, each followed by a call that needs a bearer token — the dashboard-polling
	// shape this change exists for.
	for i := range 5 {
		c, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		if _, err := c.GetCampaign(context.Background(), "123"); err != nil {
			t.Fatalf("call #%d: %v", i, err)
		}
	}

	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hits = %d across 5 resolves, want 1 — the client (and so its "+
			"cached access token) is being rebuilt per call, which is the cost LFXV2-3036 is "+
			"about; caching the decrypted credential alone does not remove it", n)
	}
}

// TestClientCache_RotatedCredentialRebuildsClient: a client is exactly as stale as the credential
// it was built from, so a rotation must produce a NEW client and a NEW token exchange.
//
// Without this, the client cache would reintroduce through the back door precisely the failure the
// credential cache was designed to prevent: a revoked credential still authenticating, held alive
// inside a cached client's token.
func TestClientCache_RotatedCredentialRebuildsClient(t *testing.T) {
	var tokenHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			tokenHits.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	first := idConn("conn-1", googleAdsTestCreds, "1234567890", 1)
	first.Provider = model.ProviderGoogleAds
	repo := &syncConnReader{row: first}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{},
		googleads.WithTokenURL(srv.URL+"/token"), googleads.WithBaseURL(srv.URL))

	c1, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := c1.GetCampaign(context.Background(), "123"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Rotate: same row, new credential, bumped version — as SetCredential does.
	rotated := idConn("conn-1", googleAdsTestCredsRotated, "1234567890", 2)
	rotated.Provider = model.ProviderGoogleAds
	repo.row = rotated

	c2, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if c2 == c1 {
		t.Fatal("the SAME client instance was returned after a credential rotation: its cached " +
			"access token was minted from the superseded credential, so a revoked credential " +
			"keeps authenticating through the client cache")
	}
	if _, err := c2.GetCampaign(context.Background(), "123"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := tokenHits.Load(); n != 2 {
		t.Errorf("token hits = %d, want 2 (one per credential generation)", n)
	}
}

const (
	googleAdsTestCreds        = `{"clientId":"c","clientSecret":"s","developerToken":"d","refreshToken":"r"}`
	googleAdsTestCredsRotated = `{"clientId":"c","clientSecret":"s2","developerToken":"d","refreshToken":"r2"}`
)

// TestClientCache_DifferentProjectsDoNotShareAClient pins that a cached client is scoped to the
// project whose connection built it.
//
// The client carries an authenticated OAuth token and a CustomerID, so serving one project's
// client to another would act on the wrong ad account under the wrong credential — the same class
// of failure as leaking the credential itself, and reachable here only through the cache key. The
// key includes the project id, and this is the test that would fail if it ever stopped doing so.
func TestClientCache_DifferentProjectsDoNotShareAClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	// Two projects, each with its own ad account — but sharing a row id and version.
	//
	// The shared id is what makes this test bind the PROJECT component of the key. With distinct
	// ids the entries differ on id alone, so the test passes even with the project stripped from
	// the key (verified by mutation: it survived until this fixture changed) and proves nothing
	// about project scoping. Ids are unique per row in Postgres, so this pairing cannot occur in
	// production; it is constructed precisely to isolate the one component under test.
	rows := map[string]*model.Connection{
		"cncf": idConn("conn-shared", googleAdsTestCreds, "1111111111", 1),
		"lfai": idConn("conn-shared", googleAdsTestCredsRotated, "2222222222", 1),
	}
	for _, r := range rows {
		r.Provider = model.ProviderGoogleAds
	}
	repo := &scopedConnReader{rows: rows}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{},
		googleads.WithTokenURL(srv.URL+"/token"), googleads.WithBaseURL(srv.URL))

	cncf, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve cncf: %v", err)
	}
	lfai, err := d.resolveGoogleAdsClient(context.Background(), "lfai", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve lfai: %v", err)
	}

	if cncf == lfai {
		t.Fatal("both projects were served the SAME client: it holds one project's OAuth token " +
			"and customer id, so the other's dispatches would act on an account it does not own")
	}
	if cncf.CustomerID() != "1111111111" || lfai.CustomerID() != "2222222222" {
		t.Errorf("customer ids = %q/%q, want each project's own account",
			cncf.CustomerID(), lfai.CustomerID())
	}
}

// TestClientCache_FallbackProjectsShareOneSystemClient pins that projects resolving through the
// LF system fallback share ONE client, and therefore one OAuth token exchange.
//
// The fallback means every project without a connection of its own runs on the SAME system row.
// credCache already keys that plaintext under model.SystemProjectID; the client built from it must
// use the same scope, because the access token is cached on the client INSTANCE. Keying the client
// by the CALLING project instead gave each fallback project its own client and its own token —
// measured at one token exchange per project, which is precisely the per-call exchange this cache
// exists to remove. This test counts hits on a real token endpoint, not decrypts, because a
// decrypt count cannot observe the token at all.
func TestClientCache_FallbackProjectsShareOneSystemClient(t *testing.T) {
	var tokens int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			atomic.AddInt64(&tokens, 1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	sys := idConn("conn-sys", googleAdsTestCreds, "9999999999", 1)
	sys.Provider = model.ProviderGoogleAds
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sys}}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{},
		googleads.WithTokenURL(srv.URL+"/token"), googleads.WithBaseURL(srv.URL))

	var first *googleads.Client
	for _, p := range []string{"alpha", "beta", "gamma", "alpha", "beta"} {
		c, err := d.resolveGoogleAdsClient(context.Background(), p, model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve %s: %v", p, err)
		}
		// Force the exchange: the token is minted lazily on first authenticated call.
		if _, err := c.ListAccessibleCustomers(context.Background()); err != nil {
			t.Fatalf("list for %s: %v", p, err)
		}
		if first == nil {
			first = c
		} else if c != first {
			t.Errorf("project %s got a DIFFERENT client for the same system row: each instance "+
				"carries its own token cache, so per-project clients re-mint the access token", p)
		}
	}
	if got := atomic.LoadInt64(&tokens); got != 1 {
		t.Errorf("token exchanges = %d, want 1: five resolves of ONE shared system row across "+
			"three projects must mint a single token", got)
	}
}

// TestCredCache_StragglerJoiningAfterFlightDoesNotRedecrypt pins the cache re-check INSIDE the
// singleflight closure.
//
// singleflight only coalesces callers whose Do calls OVERLAP. A caller that missed get() can be
// descheduled and enter Do after the leader's flight has already finished and populated the entry;
// without a re-check it starts a fresh flight and decrypts a credential that is already cached.
// Measured before the fix: five concurrent resolves cost TWO decrypts.
func TestCredCache_StragglerJoiningAfterFlightDoesNotRedecrypt(t *testing.T) {
	enc := &countingEncryptor{}
	conn := idConn("conn-1", googleAdsTestCreds, "1111111111", 1)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"p": conn}}
	s := newCredsSource(repo, enc)
	s.cache = newCredCache()

	// Populate, so the entry is live and no flight is in progress.
	if _, err := s.resolve(context.Background(), "p", model.ProviderGoogleAds); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}
	before := enc.decrypts.Load()

	// The straggler shape: a caller that has already missed get() and now enters the flight.
	key := cacheKeyFor("p", model.ProviderGoogleAds)
	got, err := s.cache.decryptOnce(key, conn.ID, conn.Version, func() (*resolved, error) {
		return s.decryptConn(context.Background(), key, "p", conn, model.ProviderGoogleAds)
	})
	if err != nil {
		t.Fatalf("straggler decryptOnce: %v", err)
	}
	if got == nil {
		t.Fatal("straggler received no credential")
	}
	if after := enc.decrypts.Load(); after != before {
		t.Errorf("decrypts went %d -> %d: a caller entering the flight after it completed "+
			"re-decrypted a credential that was already cached", before, after)
	}
}

// TestCredCache_StragglerWithSupersededVersionStillDecrypts is the other half: the in-flight
// re-check must not become a way to serve a SUPERSEDED credential. It runs under the same
// (row id, version) validation as any read, so a caller whose fresh row read saw a newer version
// misses and decrypts.
func TestCredCache_StragglerWithSupersededVersionStillDecrypts(t *testing.T) {
	enc := &countingEncryptor{}
	conn := idConn("conn-1", googleAdsTestCreds, "1111111111", 1)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"p": conn}}
	s := newCredsSource(repo, enc)
	s.cache = newCredCache()

	if _, err := s.resolve(context.Background(), "p", model.ProviderGoogleAds); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}
	before := enc.decrypts.Load()

	// A caller that read the row AFTER a rotation: same id, higher version.
	rotated := idConn("conn-1", googleAdsTestCredsRotated, "1111111111", 2)
	key := cacheKeyFor("p", model.ProviderGoogleAds)
	if _, err := s.cache.decryptOnce(key, rotated.ID, rotated.Version, func() (*resolved, error) {
		return s.decryptConn(context.Background(), key, "p", rotated, model.ProviderGoogleAds)
	}); err != nil {
		t.Fatalf("rotated decryptOnce: %v", err)
	}
	if after := enc.decrypts.Load(); after != before+1 {
		t.Errorf("decrypts went %d -> %d, want one more: the in-flight re-check must not serve "+
			"a credential the caller's own row read has already superseded", before, after)
	}
}

// TestClientCache_ColdKeyConcurrentBuildsAreCoalesced covers the burst the sequential reuse test
// cannot see: on a COLD key every caller misses, and without coalescing each builds its own client
// and each mints its own token from its own instance cache — a cold key under load behaving exactly
// as if there were no cache. That is the shape a dashboard produces when several panels load at
// once, or when a pod restarts.
//
// It drives buildOnce DIRECTLY with a build closure that blocks, rather than going through
// resolveGoogleAdsClient behind a repository barrier. That indirection was the first version of
// this test and it was only 98% binding: a Get-rendezvous releases all callers at the same instant
// but does not order them against client CONSTRUCTION, so the leader can finish and populate the
// entry before a straggler reaches the cache — measured with the no-coalescing mutant surviving 1
// run in 60. Exactly the failure mode the credential coalescing test hit twice before it was fixed
// the same way. Holding the build itself in flight makes the assertion deterministic.
func TestClientCache_ColdKeyConcurrentBuildsAreCoalesced(t *testing.T) {
	const callers = 16
	c := newClientCache()
	key := cacheKeyFor("cncf", model.ProviderGoogleAds)

	var builds atomic.Int64
	release := make(chan struct{})
	entered := make(chan struct{})
	var enterOnce sync.Once

	build := func() (any, error) {
		builds.Add(1)
		enterOnce.Do(func() { close(entered) })
		<-release // hold the flight open so every caller must join it rather than race it
		return "client", nil
	}

	var (
		wg     sync.WaitGroup
		gotMu  sync.Mutex
		got    []any
		gotErr []error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.buildOnce(key, "conn-1", 1, build)
			gotMu.Lock()
			defer gotMu.Unlock()
			if err != nil {
				gotErr = append(gotErr, err)
				return
			}
			got = append(got, v)
		}()
	}

	<-entered
	waitForAllInFlight(t, callers) // every caller is inside Do before the build completes
	close(release)
	wg.Wait()

	for _, err := range gotErr {
		t.Errorf("buildOnce: %v", err)
	}
	if n := builds.Load(); n != 1 {
		t.Errorf("builds = %d across %d concurrent cold-start callers, want 1 — construction is "+
			"not coalesced, so a cold key under a burst builds one client (and mints one OAuth "+
			"token) per caller, exactly as if there were no cache", n, callers)
	}
	if len(got) != callers {
		t.Errorf("got %d results, want %d", len(got), callers)
	}
	for _, v := range got {
		if v != "client" {
			t.Errorf("caller received %v, want the single built client", v)
		}
	}
}

// TestClientCache_ReconnectAtSameVersionRebuildsClient is the client-side counterpart to
// TestCredCache_ReconnectAtSameVersionIsNotServedFromCache.
//
// The existing client tests only rotate the credential on the SAME row, which bumps the version —
// so removing the connID comparison from clientCache.get would leave them green while a
// disconnect/reconnect (new row id, version restarting at the column DEFAULT of 1) kept serving the
// DISCONNECTED account's client and its live access token. A cached client is a cached credential
// with a token attached; it needs the same identity check, and its own test to prove it.
func TestClientCache_ReconnectAtSameVersionRebuildsClient(t *testing.T) {
	var tokenHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			tokenHits.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	first := idConn("conn-old", googleAdsTestCreds, "1111111111", 1)
	first.Provider = model.ProviderGoogleAds
	repo := &syncConnReader{row: first}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{},
		googleads.WithTokenURL(srv.URL+"/token"), googleads.WithBaseURL(srv.URL))

	c1, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := c1.GetCampaign(context.Background(), "123"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if c1.CustomerID() != "1111111111" {
		t.Fatalf("first client customer = %q, want the original account", c1.CustomerID())
	}

	// Disconnect + reconnect a DIFFERENT account: new row id, and the version restarts at 1.
	reconnected := idConn("conn-new", googleAdsTestCredsRotated, "2222222222", 1)
	reconnected.Provider = model.ProviderGoogleAds
	repo.row = reconnected

	c2, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if c2 == c1 || c2.CustomerID() == "1111111111" {
		t.Fatal("the DISCONNECTED account's client was served from cache after a reconnect: the " +
			"new row restarts at version 1, so version alone cannot tell it from the row it " +
			"replaced, and its cached token keeps acting on the account the project just removed")
	}
	if c2.CustomerID() != "2222222222" {
		t.Errorf("second client customer = %q, want the reconnected account", c2.CustomerID())
	}
	if _, err := c2.GetCampaign(context.Background(), "123"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := tokenHits.Load(); n != 2 {
		t.Errorf("token hits = %d, want 2 (one per connection)", n)
	}
}

// unhashableReader is a connReader whose TYPE is comparable but whose VALUE is not: the `any`
// field holds a slice, so reflect reports Comparable() == true and a map insert panics. Both
// connReader and domain.Encryptor are interfaces callers implement, so this shape is reachable
// from outside the package.
type unhashableReader struct {
	fakeConnReader
	tag any
}

// TestSharedCredCache_UnhashableValueFallsBackInsteadOfPanicking pins the registry's failure mode.
//
// sharedCredCache guards the registry insert with isComparable, and the guard's whole promise is
// that an implementation it cannot key gets its OWN cache rather than crashing the process.
// Checking only the TYPE does not deliver that: comparability of an interface field is decided by
// the dynamic value, so `struct{ tag any }` holding a slice passes a reflect check and then panics
// on insert — on a dispatch path, not at construction.
func TestSharedCredCache_UnhashableValueFallsBackInsteadOfPanicking(t *testing.T) {
	repo := unhashableReader{tag: []int{1, 2}}
	if !reflect.TypeOf(repo).Comparable() {
		t.Fatal("fixture no longer models the hazard: the TYPE must look comparable to reflect, " +
			"which is what makes a type-only check pass and the insert panic")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sharedCredCache panicked on a value reflect calls comparable: %v; the "+
				"fallback exists so an unkeyable implementation gets a private cache instead", r)
		}
	}()

	c1 := sharedCredCache(repo, identityEncryptor{})
	if c1 == nil {
		t.Fatal("no cache returned")
	}
	// A private cache, not a shared registry entry: two sources over the same unhashable value
	// must not be wired together, since the registry could not key them in the first place.
	c2 := sharedCredCache(repo, identityEncryptor{})
	if c1 == c2 {
		t.Error("two unkeyable sources were given the SAME cache; they cannot have been keyed, " +
			"so sharing means something other than the (repo, encryptor) identity decided it")
	}
}
