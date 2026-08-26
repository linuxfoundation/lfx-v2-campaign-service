// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// This file covers the LFXV2-3033 extension of clientCache to Reddit and Microsoft, and — in the
// second wave of the same ticket — Meta, LinkedIn and X/Twitter. Google Ads already had its own
// coverage in credcache_test.go. Each provider here gets the same three properties the Google Ads
// client cache is held to:
//
//  1. a warm key REUSES the client. For Reddit and Microsoft that means the OAuth token is minted
//     once rather than per call; for X the payoff is different — it mints no token, and what
//     reuse buys is a SHARED WRITE PACER, since X's 1-write/sec limit is per ad account and can
//     only be enforced by an instance every caller for that account shares;
//  2. a credential change (rotation, which bumps the version, and reconnect, which restarts it
//     at 1 on a new row) FORCES reconstruction — the invalidation contract;
//  3. a cold key under a concurrent burst builds ONE client, and the shared instance is safe
//     under -race.
//
// Each provider needs its own tests rather than inheriting the Google Ads ones because the safety
// argument is per-client: Reddit and Microsoft cache an access token on the instance behind their
// own mutex, X guards its pacer's next-slot instant behind writeMu, and a client that stashed
// per-call state on the receiver would be unsafe to share no matter how correct the cache is. See
// the struct comments on RedditDispatcher.clients, MicrosoftDispatcher.clients and
// TwitterDispatcher.clients.
//
// X also adds a fourth case the others do not need: its CREATE path is pinned separately
// (TestClientCache_TwitterDispatchUsesTheCachedClient), because "wired" is a claim about a
// provider while the bypasses are per-PATH — Google Ads is wired on toggle/metrics yet builds
// inline in Dispatch.

// redditCacheConn builds a Reddit connection row with an explicit id and version, so a test can
// model a rotation (same id, higher version) and a reconnect (new id, version back to 1) — the two
// cases clientCache.get distinguishes.
func redditCacheConn(id, creds, accountID string, version int64) *model.Connection {
	return &model.Connection{
		ID:                   id,
		Version:              version,
		Provider:             model.ProviderRedditAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		Status:               model.StatusActive,
	}
}

// microsoftCacheConn is redditCacheConn for Microsoft Advertising.
func microsoftCacheConn(id, creds, accountID string, version int64) *model.Connection {
	return &model.Connection{
		ID:                   id,
		Version:              version,
		Provider:             model.ProviderMicrosoftAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		Status:               model.StatusActive,
	}
}

// tokenCountingServer serves both the OAuth token endpoint and a permissive API stub, counting
// hits on the TOKEN endpoint only.
//
// Counting tokens rather than decrypts is the whole point of a CLIENT cache: the credential cache
// already removed the decrypt, and the exchange survives it because the access token lives on the
// client instance. A test that counted decrypts would pass with no client cache at all.
func tokenCountingServer(t *testing.T, apiBody string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var tokenHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token") {
			tokenHits.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, apiBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &tokenHits
}

// redditProbe drives one real call through the client so it has to authenticate.
//
// The API error is deliberately ignored: the stub server answers a body the metrics parser will
// not accept, and this helper exists only to force the OAuth exchange that happens BEFORE the API
// call. What is asserted is the token count, never the call's result. The campaign id must satisfy
// the client's local word-character guard, which runs before any network I/O — an id that fails it
// returns early and mints no token at all, which is how the first version of these tests measured
// zero exchanges against a perfectly working cache.
func redditProbe(t *testing.T, c *reddit.Client) {
	t.Helper()
	_, _ = c.GetCampaignMetrics(context.Background(), "t2_camp1", model.MetricsWindowLast7Days)
}

// microsoftProbe is redditProbe for Microsoft. Its id and the connection's ACCOUNT id must both be
// digits only — the client rejects either locally, before authenticating.
//
// GetCampaignMetrics submits to the REPORTING origin (c.reportingBaseURL), not the Campaign
// Management one, so every caller must point microsoft.WithReportingBaseURL at the stub as well
// as WithBaseURL — see newMicrosoftCacheDispatcher. Stubbing only WithBaseURL left this probe
// dialling https://reporting.api.bingads.microsoft.com on every `make test` and in CI: the
// assertions still passed, because the probe ignores its error and the token exchange it exists
// to force had already happened, so the suite was measuring a live network failure rather than
// the cache. It cost ~2.8s per run here and would fail closed in a sandbox with no egress.
func microsoftProbe(t *testing.T, c *microsoft.Client) {
	t.Helper()
	_, _ = c.GetCampaignMetrics(context.Background(), "12345", model.MetricsWindowLast7Days)
}

// newMicrosoftCacheDispatcher builds the dispatcher every Microsoft client-cache test uses,
// pointing the token endpoint and ALL THREE of the client's API origins at the stub. It exists so
// the reporting override cannot be forgotten in one test and silently reintroduce the live call:
// microsoft.Client splits its API across three hosts (Campaign Management, Customer Management and
// Reporting), and an un-overridden origin falls back to the production default rather than failing
// loudly.
//
// extra carries per-test options — microsoft.WithHTTPClient, for one — appended AFTER the origin
// overrides so a test can ADD to them without restating them. That parameter is the whole point:
// TestClientCache_MicrosoftProbeStaysOnTheStub used to re-spell all four origin options inline
// purely because it also needed a custom http.Client, which put the one test that exists to CATCH
// a missing reporting override outside the helper that prevents one. Measured: with
// WithReportingBaseURL deleted from this helper, that test stayed GREEN while the other Microsoft
// probes dialled reporting.api.bingads.microsoft.com (the package's Microsoft tests went from 1.3s
// to 5.0s). Every Microsoft dispatcher in this file goes through here.
func newMicrosoftCacheDispatcher(repo connReader, srv *httptest.Server, extra ...microsoft.Option) *MicrosoftDispatcher {
	opts := []microsoft.Option{
		microsoft.WithTokenURL(srv.URL + "/token"),
		microsoft.WithBaseURL(srv.URL),
		microsoft.WithCustomerBaseURL(srv.URL),
		microsoft.WithReportingBaseURL(srv.URL),
	}
	return NewMicrosoftDispatcher(repo, identityEncryptor{}, append(opts, extra...)...)
}

// loopbackOnlyClient returns an *http.Client whose dialer REFUSES any address outside loopback,
// and a counter of the refusals.
//
// This is the standing guard for the whole file: every URL these tests drive must point at the
// httptest stub, and the failure mode when one does not is silent. microsoft.Client splits its API
// across three hosts and each un-overridden origin falls back to a real Microsoft endpoint, while
// the probes deliberately discard their API error — so a missed override produced a green test
// that dialled production on every run. Asserting on elapsed time would be flaky; refusing the
// dial makes the escape an explicit, deterministic failure at the point it happens.
func loopbackOnlyClient(t *testing.T) (*http.Client, *atomic.Int64) {
	t.Helper()
	var offBox atomic.Int64
	var d net.Dialer
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
					offBox.Add(1)
					return nil, fmt.Errorf("test dialled non-loopback address %q: a client origin is "+
						"not pointed at the httptest stub and this call is reaching the real provider", addr)
				}
				return d.DialContext(ctx, network, addr)
			},
		},
	}, &offBox
}

// TestClientCache_MicrosoftProbeStaysOnTheStub pins finding (1): microsoftProbe drives
// GetCampaignMetrics, which targets the REPORTING origin rather than the Campaign Management one
// the other overrides cover. With only WithBaseURL+WithTokenURL stubbed this probe dialled
// https://reporting.api.bingads.microsoft.com on every `make test` and in CI.
//
// The test still PASSED in that state — the probe ignores its error, so the cache assertions were
// resting on a live network failure instead of on the cache. That is why this asserts the DIAL and
// not the metrics result: the escape is invisible to every other assertion in the file.
func TestClientCache_MicrosoftProbeStaysOnTheStub(t *testing.T) {
	srv, _ := tokenCountingServer(t, `{"value":[]}`)
	hc, offBox := loopbackOnlyClient(t)

	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1)}
	// Through the helper, NOT inline. This is the test whose entire job is to catch a missing
	// origin override, so it is the last one that should carry its own copy of them: with the four
	// options re-spelled here, deleting WithReportingBaseURL from newMicrosoftCacheDispatcher left
	// this test green while every other Microsoft probe in the file dialled production.
	d := newMicrosoftCacheDispatcher(repo, srv, microsoft.WithHTTPClient(hc))

	c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	microsoftProbe(t, c)

	if n := offBox.Load(); n != 0 {
		t.Errorf("the Microsoft probe dialled a non-loopback address %d time(s): an origin is "+
			"falling back to a production Microsoft host, so `make test` and CI issue real "+
			"outbound requests and the cache assertions rest on a network error", n)
	}
}

// TestClientCache_RedditReusesClientAndToken pins the reuse this change exists for on the Reddit
// path: repeated resolves for the same connection hand back the SAME client, so the OAuth token is
// exchanged once instead of once per call.
//
// Before this change RedditDispatcher built a fresh reddit.Client on every resolve, and because
// the access token is cached on the instance (reddit.Client.cachedToken), every dispatch, toggle
// and metrics read re-minted it.
func TestClientCache_RedditReusesClientAndToken(t *testing.T) {
	srv, tokenHits := tokenCountingServer(t, `{"data":{"id":"t2_acct"}}`)

	repo := &syncConnReader{row: redditCacheConn("conn-1", goodRedditCreds, "t2_acct", 1)}
	d := NewRedditDispatcher(repo, identityEncryptor{},
		reddit.WithBaseURL(srv.URL+"/api/v3"), reddit.WithTokenURL(srv.URL+"/token"))

	var first *reddit.Client
	for i := range 5 {
		c, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds, d.creds.resolve)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		// Drive a real call so the client actually needs a token; a client that is never used
		// never exchanges one, and the token count would be trivially 0 with or without a cache.
		redditProbe(t, c)
		if i == 0 {
			first = c
			continue
		}
		if c != first {
			t.Fatalf("resolve #%d returned a NEW client for an unchanged connection: the client "+
				"cache is not being consulted, so every call re-mints an OAuth token", i)
		}
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times across 5 resolves of one unchanged connection, want 1 "+
			"— the client is being rebuilt, and each instance mints its own token", n)
	}
}

// TestClientCache_RedditRotationForcesRebuild is the invalidation contract on the Reddit path: a
// client built from credential version N must not survive a bump to N+1.
//
// This is the property that makes a client cache safe rather than a security hole. A rotated or
// revoked credential is only actually revoked if the CLIENT built from it also stops being served
// — a cached client carries a live access token minted from the old credential, so serving it past
// a rotation is exactly as dangerous as serving the old credential itself.
func TestClientCache_RedditRotationForcesRebuild(t *testing.T) {
	srv, tokenHits := tokenCountingServer(t, `{"data":{"id":"t2_acct"}}`)

	repo := &syncConnReader{row: redditCacheConn("conn-1", goodRedditCreds, "t2_acct", 1)}
	d := NewRedditDispatcher(repo, identityEncryptor{},
		reddit.WithBaseURL(srv.URL+"/api/v3"), reddit.WithTokenURL(srv.URL+"/token"))

	c1, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Drive a call so this client actually mints a token. A rebuild is only observable as a
	// SECOND token exchange if the first client ever authenticated.
	redditProbe(t, c1)

	// Rotate the credential on the SAME row: every mutating statement in ConnectionRepo bumps
	// version, so the new credential arrives as version 2.
	repo.row = redditCacheConn("conn-1", `{"ClientID":"cid2","ClientSecret":"sec2","RefreshToken":"rt2"}`, "t2_acct", 2)

	c2, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("post-rotation resolve: %v", err)
	}
	if c2 == c1 {
		t.Fatal("the pre-rotation client was served from cache after the credential version was " +
			"bumped: a client built from a superseded credential keeps acting with its live " +
			"access token, so the rotation did not actually take effect")
	}
	redditProbe(t, c2)

	// A reconnect is the case version alone cannot catch: Delete soft-deletes and Create INSERTs a
	// fresh row, so a disconnect/reconnect can present a DIFFERENT row at the SAME key carrying a
	// version the cache has already seen. Only the row id separates them.
	//
	// The fixture has to be built so the row id is the ONLY thing that can force the rebuild,
	// which takes two things beyond changing the id:
	//
	//   - the credential and the account id are held CONSTANT, so neither the upstream
	//     credential cache nor differing plaintext can force it; and
	//   - the version MATCHES the version the cached entry carries. This is the part that is
	//     easy to get wrong: the rotation above left a version-2 entry cached, so a reconnect
	//     landing at version 1 would miss on the VERSION comparison alone and never consult
	//     connID at all. Reconnecting at version 2 makes (version) agree and (row id) the sole
	//     discriminator — which is exactly the state the connID check exists for.
	//
	// Verified by mutation: deleting `|| entry.connID != connID` from clientCache.get fails this
	// test. It did NOT fail an earlier fixture that reconnected at version 1 with a different
	// account id, because the version mismatch was silently doing the work.
	repo.row = redditCacheConn("conn-2", goodRedditCreds, "t2_acct", 2)

	c3, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("post-reconnect resolve: %v", err)
	}
	if c3 == c2 || c3 == c1 {
		t.Fatal("a previously cached client was served after a disconnect/reconnect: the new row " +
			"carries the same version as the row it replaced, so version alone cannot tell them " +
			"apart, and the cached client keeps acting on the connection the project just removed")
	}
	redditProbe(t, c3)
	// Three DISTINCT clients each minted their own token. Asserting the count — not just
	// pointer inequality — is what proves the rebuild reached the credential: a cache that
	// returned a fresh wrapper around a shared token would pass the identity checks above.
	if n := tokenHits.Load(); n != 3 {
		t.Errorf("token endpoint hit %d times across the original, the rotated and the "+
			"reconnected credential, want 3 — each rebuild must authenticate with its own "+
			"credential rather than inheriting a token minted from a superseded one", n)
	}
}

// TestClientCache_RedditColdKeyConcurrentBuildsAreCoalesced covers the burst the sequential reuse
// test cannot see: on a COLD key every caller misses, and without coalescing each builds its own
// client and each mints its own token — a cold key under load behaving exactly as if there were no
// cache. It is also the -race exercise for SHARING one reddit.Client across concurrent callers.
func TestClientCache_RedditColdKeyConcurrentBuildsAreCoalesced(t *testing.T) {
	const callers = 16
	srv, tokenHits := tokenCountingServer(t, `{"data":{"id":"t2_acct"}}`)

	// The barrier puts every caller inside the cache-miss window simultaneously, which is the
	// precondition the coalescing assertion rests on and which no sleep can guarantee.
	repo := &syncConnReader{row: redditCacheConn("conn-1", goodRedditCreds, "t2_acct", 1), barrier: callers}
	d := NewRedditDispatcher(repo, identityEncryptor{},
		reddit.WithBaseURL(srv.URL+"/api/v3"), reddit.WithTokenURL(srv.URL+"/token"))

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  []*reddit.Client
		errs []error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds, d.creds.resolve)
			// The lock covers ONLY the append to the shared slices. It must NOT span the probe:
			// holding it across redditProbe serialized all 16 callers, so the "concurrent" traffic
			// this test exists to generate never overlapped and -race had nothing to observe.
			// Verified by mutation — with the probe inside the critical section, deleting the
			// c.mu.Lock()/Unlock() pairs from reddit.Client.refreshToken left this test PASSING
			// under -race.
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			got = append(got, c)
			mu.Unlock()

			// Exercise the SHARED instance concurrently, OUTSIDE the bookkeeping lock, so -race
			// sees real overlapping traffic through the client's token mutex rather than
			// construction alone.
			redditProbe(t, c)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("resolveRedditClient: %v", err)
	}
	if len(got) != callers {
		t.Fatalf("got %d clients, want %d", len(got), callers)
	}
	for i, c := range got {
		if c != got[0] {
			t.Fatalf("caller %d received a different client instance: construction is not "+
				"coalesced, so a cold key under a burst builds one client (and mints one OAuth "+
				"token) per caller, exactly as if there were no cache", i)
		}
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times across %d concurrent cold-start callers, want 1", n, callers)
	}
}

// TestClientCache_MicrosoftReusesClientAndToken is the Reddit reuse test on the Microsoft path.
//
// Microsoft needs its own coverage rather than inheriting Reddit's: it is the client with
// multi-customer discovery, so it was the one where a customer id that VARIED per caller would
// have made a shared instance serve one caller against another caller's customer. The configured
// customer IS held on the receiver (c.account.CustomerID; doCustomerRequest reads it rather than
// taking it as an argument) — sharing is safe because that AccountConfig is immutable and the
// cache key pins the connection row id and version, while per-customer discovery runs on a
// separate zero-AccountConfig client that bypasses the cache. See the MicrosoftDispatcher.clients
// comment.
func TestClientCache_MicrosoftReusesClientAndToken(t *testing.T) {
	srv, tokenHits := tokenCountingServer(t, `{"value":[]}`)

	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1)}
	d := newMicrosoftCacheDispatcher(repo, srv)

	var first *microsoft.Client
	for i := range 5 {
		c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		microsoftProbe(t, c)
		if i == 0 {
			first = c
			continue
		}
		if c != first {
			t.Fatalf("resolve #%d returned a NEW client for an unchanged connection: the client "+
				"cache is not being consulted, so every call re-mints an OAuth token", i)
		}
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times across 5 resolves of one unchanged connection, want 1 "+
			"— the client is being rebuilt, and each instance mints its own token", n)
	}
}

// TestClientCache_MicrosoftRotationForcesRebuild is the invalidation contract on the Microsoft
// path — the same two cases as the Reddit test: a rotation (version bump on the same row) and a
// disconnect/reconnect (new row id, version restarting at 1).
func TestClientCache_MicrosoftRotationForcesRebuild(t *testing.T) {
	srv, tokenHits := tokenCountingServer(t, `{"value":[]}`)

	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1)}
	d := newMicrosoftCacheDispatcher(repo, srv)

	c1, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Drive a call so this client actually mints a token. A rebuild is only observable as a
	// SECOND token exchange if the first client ever authenticated.
	microsoftProbe(t, c1)

	rotated := `{"ClientID":"cid2","ClientSecret":"csec2","DeveloperToken":"dev2","RefreshToken":"rt2"}`
	repo.row = microsoftCacheConn("conn-1", rotated, "111111", 2)

	c2, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
	if err != nil {
		t.Fatalf("post-rotation resolve: %v", err)
	}
	if c2 == c1 {
		t.Fatal("the pre-rotation client was served from cache after the credential version was " +
			"bumped: a client built from a superseded credential keeps acting with its live " +
			"access token, so the rotation did not actually take effect")
	}
	microsoftProbe(t, c2)

	// The reconnect step: same credential, same account id, and the SAME version the cached entry
	// carries — so the row id is the only thing left that can force the rebuild. See the Reddit
	// counterpart for why reconnecting at version 1 here would leave the connID check unpinned:
	// the rotation above cached version 2, so version alone would already miss.
	repo.row = microsoftCacheConn("conn-2", goodMicrosoftCreds, "111111", 2)

	c3, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
	if err != nil {
		t.Fatalf("post-reconnect resolve: %v", err)
	}
	if c3 == c2 || c3 == c1 {
		t.Fatal("a previously cached client was served after a disconnect/reconnect: the new row " +
			"carries the same version as the row it replaced, so version alone cannot tell them " +
			"apart, and the cached client keeps acting on the connection the project just removed")
	}
	microsoftProbe(t, c3)
	// Three DISTINCT clients each minted their own token — see the Reddit counterpart for why
	// the COUNT matters beyond pointer inequality.
	if n := tokenHits.Load(); n != 3 {
		t.Errorf("token endpoint hit %d times across the original, the rotated and the "+
			"reconnected credential, want 3 — each rebuild must authenticate with its own "+
			"credential rather than inheriting a token minted from a superseded one", n)
	}
}

// TestClientCache_MicrosoftColdKeyConcurrentBuildsAreCoalesced is the Reddit cold-key burst test
// on the Microsoft path, and the -race exercise for sharing one microsoft.Client.
func TestClientCache_MicrosoftColdKeyConcurrentBuildsAreCoalesced(t *testing.T) {
	const callers = 16
	srv, tokenHits := tokenCountingServer(t, `{"value":[]}`)

	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1), barrier: callers}
	d := newMicrosoftCacheDispatcher(repo, srv)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  []*microsoft.Client
		errs []error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
			// Lock scope: bookkeeping only, never the probe — see the Reddit counterpart for the
			// mutation that proved a probe inside the critical section makes this test serial and
			// therefore blind to a missing production mutex.
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			got = append(got, c)
			mu.Unlock()

			microsoftProbe(t, c)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("resolveMicrosoftClient: %v", err)
	}
	if len(got) != callers {
		t.Fatalf("got %d clients, want %d", len(got), callers)
	}
	for i, c := range got {
		if c != got[0] {
			t.Fatalf("caller %d received a different client instance: construction is not "+
				"coalesced, so a cold key under a burst builds one client (and mints one OAuth "+
				"token) per caller, exactly as if there were no cache", i)
		}
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times across %d concurrent cold-start callers, want 1", n, callers)
	}
}

// TestClientCache_MicrosoftDispatchUsesTheCachedClient pins the WIRING, which none of the tests
// above reach: they all drive resolveMicrosoftClient (the toggle/metrics entry point), while
// Dispatch builds its client on a separate line of its own.
//
// Reverting that line to a direct microsoft.NewClient(...) is a COMPILING change that left the
// entire suite green — the cache was fully tested and simply not used by the create path, which is
// the path the cache exists for (a dispatch burst re-minting a token per campaign). What binds it
// is the token COUNT across two dispatches of an unchanged connection: cached construction mints
// once, direct construction mints per call.
func TestClientCache_MicrosoftDispatchUsesTheCachedClient(t *testing.T) {
	var tokenHits atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(apiSrv.Close)

	// One dispatcher, one unchanged connection row: the cache key is identical across both calls,
	// so a cached build is reused and only the FIRST call reaches the token endpoint.
	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1)}
	// Two servers here (token counted separately from the API), which is why this one cannot take
	// newMicrosoftCacheDispatcher's single-server shape. The discipline it enforces still applies:
	// ALL THREE API origins are pointed at apiSrv, so no un-overridden origin can fall back to a
	// production Microsoft host.
	d := NewMicrosoftDispatcher(repo, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL),
		microsoft.WithBaseURL(apiSrv.URL),
		microsoft.WithCustomerBaseURL(apiSrv.URL),
		microsoft.WithReportingBaseURL(apiSrv.URL))

	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50}}`)
	for i := range 2 {
		if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg); err != nil {
			t.Fatalf("Dispatch #%d: %v", i, err)
		}
	}

	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times across 2 dispatches of one unchanged connection, want 1 "+
			"— Dispatch is constructing its client directly instead of going through "+
			"cachedMicrosoftClient, so the create path re-mints an OAuth token per campaign and the "+
			"client cache does nothing for the burst it exists to collapse", n)
	}
}

// TestClientCache_GoogleAdsDispatchBypassesTheCache pins the ROSTER, not a behaviour change.
//
// clientCache's doc comment and docs/knowledge/code/internal-dispatch.md are the single source of
// truth for which Google Ads paths bypass the client cache, and both used to name only two
// exclusions — the account-agnostic discovery client and adoption's owned-connection path — while
// GoogleAdsDispatcher.Dispatch has always built its client inline (googleads.go, the
// googleads.NewClient call inside Dispatch). A reader of either document would conclude that a
// dispatch burst reuses one cached client and mints one token; it does not.
//
// This asserts the CURRENT behaviour so the two documents can state it accurately, and so the day
// Dispatch is wired to cachedGoogleAdsClient this test fails and forces the rosters to be updated
// in the same change. It is the mirror image of TestClientCache_MicrosoftDispatchUsesTheCachedClient
// above: Microsoft's create path IS cached, Google Ads' is not, and the difference between the two
// providers is exactly what the rosters were silently getting wrong.
//
// Two dispatches of ONE unchanged connection: a cached build would mint a single token (as the
// Microsoft test asserts), a per-call build mints one each.
func TestClientCache_GoogleAdsDispatchBypassesTheCache(t *testing.T) {
	var tokenHits atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)

	// syncConnReader (not fakeConnReader) so the row carries an explicit id and version: those two
	// are the cache key, and a row without them cannot demonstrate a cache HIT even if one occurred.
	repo := &syncConnReader{row: googleAdsCacheConn("conn-1", goodGoogleAdsCreds, "1234567890", 1)}
	d := NewGoogleAdsDispatcher(repo, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL))

	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50}}`)
	for i := range 2 {
		if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
			t.Fatalf("Dispatch #%d: %v", i, err)
		}
	}

	// 2, not 1. If this ever reads 1, Dispatch has been wired to cachedGoogleAdsClient — which is a
	// welcome change, and the point of asserting it here is that the change must also update
	// clientCache's roster comment and docs/knowledge/code/internal-dispatch.md, which currently
	// describe this path as a bypass.
	if n := tokenHits.Load(); n != 2 {
		t.Errorf("token endpoint hit %d times across 2 Google Ads dispatches of one unchanged "+
			"connection, want 2 — Dispatch builds its client inline rather than through "+
			"cachedGoogleAdsClient, and the clientCache roster comment plus "+
			"docs/knowledge/code/internal-dispatch.md must both say so; if this path is now "+
			"cached, update both rosters in the same change as the wiring", n)
	}
}

// googleAdsCacheConn is redditCacheConn for Google Ads: an explicit row id and version, which are
// what clientCache keys and validates on.
func googleAdsCacheConn(id, creds, accountID string, version int64) *model.Connection {
	return &model.Connection{
		ID:                   id,
		Version:              version,
		Provider:             model.ProviderGoogleAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		Status:               model.StatusActive,
	}
}

// TestClientCache_MicrosoftAccountIsReceiverStateNotPerCall pins the ACTUAL reason sharing a
// cached microsoft.Client is safe, because the reason recorded in this package's comments was
// wrong for two releases: they claimed the account/customer id "travels as a per-call argument"
// to doCustomerRequest. It does not. doCustomerRequest takes no such parameter, and the account
// headers are read off the receiver's immutable AccountConfig (client.go, CustomerAccountId /
// CustomerId request headers).
//
// That distinction IS the safety argument, so it is pinned rather than asserted in prose. If the
// id really were per-call, sharing would be safe for a reason that does not hold here. The
// property that actually holds: the account is fixed by the immutable AccountConfig at
// construction, so every request a given cached client emits carries the SAME account, and the
// cache key (row id + version) is what guarantees its callers all want that account.
//
// The assertion is independent of the constant under test: it never reads the field back off the
// client. It observes the CustomerAccountId header the SERVER received, requires the requests to
// agree with EACH OTHER across two separate resolves, and compares them to the id the connection
// row was built with — an input, not a copy of the implementation's own state.
func TestClientCache_MicrosoftAccountIsReceiverStateNotPerCall(t *testing.T) {
	const wantAccount = "222222"

	var mu sync.Mutex
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"t","expires_in":3600}`)
			return
		}
		mu.Lock()
		seen = append(seen, r.Header.Get("CustomerAccountId"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":[]}`)
	}))
	defer srv.Close()

	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, wantAccount, 1)}
	d := newMicrosoftCacheDispatcher(repo, srv)

	// Two independent resolves of the SAME identity: the second must hit the cached client.
	for i := range 2 {
		c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds, nil)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		microsoftProbe(t, c)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("server saw %d API requests, want at least 2; the probes did not reach the stub", len(seen))
	}
	for i, got := range seen {
		if got != wantAccount {
			t.Fatalf("request %d carried CustomerAccountId %q, want %q: the account is receiver "+
				"state fixed by the immutable AccountConfig, so every request from a shared "+
				"cached client must carry the SAME id", i, got, wantAccount)
		}
	}
}

// TestClientCache_MicrosoftConcurrentDispatchSharesOneGeoCache pins the invariant this PR's
// clientCache actually introduced, at the layer that introduced it.
//
// The claim on MicrosoftDispatcher.clients is that c.geo.mu was PROMOTED from defensive to
// load-bearing: before the cache, Dispatch built a client per call, so the geo snapshot was
// per-create and never shared; with the cache, concurrent Dispatch calls on one connection reach
// ONE client and therefore one geo cache. Every other geo test in the tree drives the client
// directly (internal/platform/microsoft.TestGeoLocations_CachedAcrossCallsAndCoalesced), and the
// only dispatch-level concurrency test drives GetCampaignMetrics, which never touches geo. So the
// sharing this PR creates was documented at the dispatch layer and pinned only below it.
//
// What binds it: cfg.GeoTargets is the ONLY route into geoLocationsSnapshot from Dispatch, and the
// locations file is multi-MiB — so the download COUNT across N concurrent creates is the
// observable. One download means the callers shared a client and coalesced; N means each create
// built its own.
func TestClientCache_MicrosoftConcurrentDispatchSharesOneGeoCache(t *testing.T) {
	const callers = 8

	var (
		tokenHits atomic.Int64
		downloads atomic.Int64
	)

	// The locations file is served from a path on the SAME stub, and its URL is handed back by
	// GeoLocationsFileUrl/Query. Counting downloads (not Query hits) is deliberate: the Query step
	// is a cheap read that is legitimately repeated, while the FILE is the multi-MiB object whose
	// duplication the shared cache exists to prevent.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/token"):
			tokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
		case strings.HasSuffix(p, "/geofile"):
			downloads.Add(1)
			w.Header().Set("Content-Type", "text/csv")
			// The vendor's literal header, spelled out rather than derived from the parser's own
			// column constants: a fixture generated from the parser cannot falsify the parser.
			_, _ = io.WriteString(w, "Location Id,Bing Display Name,Location Type,Replaces,Status,AdWords Location Id\n"+
				"190,United States,Country,,Active,2840\n")
		case strings.HasSuffix(p, "/GeoLocationsFileUrl/Query"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"FileUrl":%q,"FileUrlExpiryTimeUtc":"2030-01-01T00:00:00Z","LastModifiedTimeUtc":"2026-06-05T18:43:00Z"}`,
				srv.URL+"/geofile"))
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/CampaignCriterions/QueryByIds"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"CampaignCriterions":[],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/CampaignCriterions"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"CampaignCriterionIds":[9001],"NestedPartialErrors":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	// The barrier holds every caller inside Get until all of them have arrived, so the burst hits a
	// COLD cache key together — which is the only arrangement under which the geo cache is raced
	// rather than trivially warm.
	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1), barrier: callers}
	d := newMicrosoftCacheDispatcher(repo, srv)

	// GeoTargets is what makes this a geo test: without it CreateCampaign never calls
	// resolveGeoTargets and the locations file is never fetched at all.
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50,"geoTargets":["US"]}}`)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("Dispatch: %v", err)
	}

	// The load-bearing assertion. One download across N concurrent creates is only possible if all
	// N reached the same client (the clientCache) AND coalesced on its geo single-flight (c.geo.mu
	// + c.geo.inflight). Wiring Dispatch back to a direct microsoft.NewClient makes this N.
	if n := downloads.Load(); n != 1 {
		t.Errorf("locations file downloaded %d times across %d concurrent dispatches of one "+
			"unchanged connection, want 1 — concurrent creates are not sharing one cached client's "+
			"geo snapshot, so each create re-downloads the multi-MiB locations file", n, callers)
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times across %d concurrent dispatches, want 1", n, callers)
	}
}

// twitterCacheConn is redditCacheConn for X (Twitter) Ads.
func twitterCacheConn(id, creds, accountID string, version int64) *model.Connection {
	return &model.Connection{
		ID:                   id,
		Version:              version,
		Provider:             model.ProviderTwitterAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"funding_instrument_id": "fi1"},
		Status:               model.StatusActive,
	}
}

// twitterCacheCampaign is the persisted campaign the toggle/metrics resolve path reads the
// creation account from.
func twitterCacheCampaign(accountID string) *model.Campaign {
	return &model.Campaign{
		Platform:           model.ProviderTwitterAds,
		PlatformCampaignID: "cmp1",
		// The creation account lives in the untagged Result blob (see
		// twitterCreationAccountID), not in a column.
		Result: json.RawMessage(`{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"` + accountID + `"}`),
	}
}

// TestClientCache_TwitterReusesClient pins the reuse leg of LFXV2-3033 on the X path: repeated
// resolves for the same unchanged connection hand back the SAME client.
//
// The stake here is NOT a saved token exchange — X signs each request with stored OAuth 1.0a
// credentials and mints nothing at construction. It is the WRITE PACER. twitter.Client bounds
// writes to 1/sec on the instance, so that budget is only enforced if the callers working
// against an account share one instance; a client rebuilt per resolve gives each caller a fresh
// pacer and the aggregate rate scales with the number of callers.
func TestClientCache_TwitterReusesClient(t *testing.T) {
	repo := &syncConnReader{row: twitterCacheConn("conn-1", goodTwitterCreds, "acc1", 1)}
	d := NewTwitterDispatcher(repo, identityEncryptor{}, twitter.WithWriteDelay(0))

	var first *twitter.Client
	for i := range 5 {
		c, err := d.resolveTwitterClient(context.Background(), "cncf", model.ProviderTwitterAds,
			twitterCacheCampaign("acc1"))
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		if i == 0 {
			first = c
			continue
		}
		if c != first {
			t.Fatalf("resolve #%d returned a NEW client for an unchanged connection: the client "+
				"cache is not being consulted, so concurrent callers get independent write "+
				"pacers and together exceed X's 1 write/sec account budget", i)
		}
	}
}

// TestClientCache_TwitterRotationForcesRebuild is the invalidation contract on the X path: a
// client built from credential version N must not survive a bump to N+1. A cached client holds
// the OAuth 1.0a credential it signs every request with, so serving it past a rotation is
// exactly as dangerous as serving the superseded credential itself.
func TestClientCache_TwitterRotationForcesRebuild(t *testing.T) {
	repo := &syncConnReader{row: twitterCacheConn("conn-1", goodTwitterCreds, "acc1", 1)}
	d := NewTwitterDispatcher(repo, identityEncryptor{}, twitter.WithWriteDelay(0))

	c1, err := d.resolveTwitterClient(context.Background(), "cncf", model.ProviderTwitterAds,
		twitterCacheCampaign("acc1"))
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Rotate on the SAME row: every mutating statement in ConnectionRepo bumps version.
	repo.row = twitterCacheConn("conn-1",
		`{"ConsumerKey":"ck2","ConsumerSecret":"cs2","AccessToken":"at2","AccessTokenSecret":"ats2"}`,
		"acc1", 2)

	c2, err := d.resolveTwitterClient(context.Background(), "cncf", model.ProviderTwitterAds,
		twitterCacheCampaign("acc1"))
	if err != nil {
		t.Fatalf("post-rotation resolve: %v", err)
	}
	if c2 == c1 {
		t.Fatal("the pre-rotation client was served from cache after the credential version was " +
			"bumped: it still signs requests with the superseded OAuth 1.0a credential, so the " +
			"rotation did not actually take effect")
	}

	// A reconnect is the case version alone cannot catch: Delete soft-deletes and Create INSERTs
	// a fresh row, so a different row can present at the SAME key carrying a version already
	// seen. The credential and account id are held CONSTANT and the version MATCHES the cached
	// entry's, making the row id the sole discriminator — the state the connID check exists for.
	repo.row = twitterCacheConn("conn-2", goodTwitterCreds, "acc1", 2)

	c3, err := d.resolveTwitterClient(context.Background(), "cncf", model.ProviderTwitterAds,
		twitterCacheCampaign("acc1"))
	if err != nil {
		t.Fatalf("post-reconnect resolve: %v", err)
	}
	if c3 == c2 {
		t.Fatal("a reconnect at the same version was served the previous row's client: only the " +
			"row id separates a reconnect from the row it replaced")
	}
}

// TestClientCache_TwitterColdKeyConcurrentBuildsAreCoalesced covers the burst a warm-key test
// cannot see: N callers that ALL miss must build ONE client, not N. Without singleflight
// coalescing a cold key behaves like no cache at all under a dispatch burst — and for X that
// means N independent write pacers at the exact moment concurrency is highest.
//
// Three things here were arrived at by mutation rather than by reasoning, and all are
// load-bearing. Each fixed a version of this test that passed against a singleflight-free
// buildOnce:
//
//  1. It resolves ONCE up front and shares the result. Resolving per goroutine coalesces at
//     decryptOnce FIRST, so the credential-cache leader completes its build and put before the
//     followers reach buildOnce, which then serves them a WARM entry — the test was verifying
//     the credential cache, not this one.
//  2. It COUNTS CONSTRUCTIONS instead of comparing instance identity. Identity (what the
//     Reddit/Microsoft/Meta versions assert) only detects a missing singleflight when the race
//     is actually LOST. Those providers build a client that performs a token exchange, so their
//     window is wide; twitter.NewClient is a struct literal with no I/O, so the leader's put
//     beats every follower and all 16 receive the same instance either way.
//  3. It BLOCKS INSIDE build() on a barrier. Counting alone was still only ~3/10 against the
//     mutant, because without coalescing the leader's build+put still usually completes before
//     the next caller calls get(). The counting Option runs inside twitter.NewClient, i.e.
//     inside build(), i.e. inside the leader's critical section — so holding it there until
//     every caller has arrived makes both outcomes deterministic. WITH coalescing exactly one
//     build starts, the others block in the flight, and the barrier is released by the timeout
//     path below. WITHOUT it all 16 enter build, the barrier fills immediately, and the count
//     is 16.
func TestClientCache_TwitterColdKeyConcurrentBuildsAreCoalesced(t *testing.T) {
	const callers = 16
	repo := &syncConnReader{row: twitterCacheConn("conn-1", goodTwitterCreds, "acc1", 1)}

	// twitter.Option runs once per NewClient call, so it is an exact construction counter AND a
	// hook inside the leader's critical section.
	var (
		builds  atomic.Int64
		entered = make(chan struct{}, callers)
	)
	countingOpt := func(*twitter.Client) {
		builds.Add(1)
		entered <- struct{}{}
		// Hold this construction open until either every caller has entered build (the
		// uncoalesced case) or the deadline proves they cannot (the coalesced case, where the
		// other 15 are parked in the singleflight and will never arrive).
		if len(entered) < callers {
			// stop ends whichever arm loses: the poller must not outlive this construction,
			// and the timer must not outlive it either. Without this both leak per build, and
			// under -count they accumulate across iterations for the rest of the package run.
			stop := make(chan struct{})
			deadline := time.NewTimer(150 * time.Millisecond)
			select {
			case <-deadline.C:
			case <-allEntered(entered, callers, stop):
			}
			close(stop)
			deadline.Stop()
		}
	}
	d := NewTwitterDispatcher(repo, identityEncryptor{}, twitter.WithWriteDelay(0), countingOpt)

	// Resolve once, off the same path resolveTwitterClient uses, so every caller enters
	// cachedTwitterClient inside the cache-miss window together. No barrier on repo.Get: only
	// one Get happens now, so an N-party barrier would deadlock waiting for arrivals that never
	// come. The start channel aligns the callers on buildOnce, the window under test.
	res, rerr := d.creds.resolveExisting(context.Background(), "cncf", model.ProviderTwitterAds,
		twitterCreationAccountID(twitterCacheCampaign("acc1")))
	if rerr != nil {
		t.Fatalf("pre-resolve: %v", rerr)
	}
	creds, accountID, verr := validateTwitterConnection("cncf", res)
	if verr != nil {
		t.Fatalf("pre-validate: %v", verr)
	}
	fundingID := strings.TrimSpace(res.providerConfig["funding_instrument_id"])

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []*twitter.Client
	)
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c := d.cachedTwitterClient("cncf", model.ProviderTwitterAds, res, creds, accountID, fundingID)
			// The lock covers ONLY the append: holding it across anything else would serialize
			// the callers and -race would observe no overlap.
			mu.Lock()
			got = append(got, c)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if n := builds.Load(); n != 1 {
		t.Errorf("%d concurrent cold-key callers performed %d constructions, want 1 — buildOnce "+
			"is not coalescing, so a dispatch burst builds one client per caller and each carries "+
			"its own write pacer, multiplying the account's 1 write/sec budget by the burst size",
			callers, n)
	}
	if len(got) != callers {
		t.Fatalf("got %d clients, want %d", len(got), callers)
	}
	// Identity still has to hold: every caller must be handed the ONE client that was built.
	for i, c := range got {
		if c != got[0] {
			t.Fatalf("caller %d received a different client instance despite a single construction", i)
		}
	}
}

// allEntered returns a channel closed once ch holds n items. Used to release the construction
// barrier promptly in the UNCOALESCED case rather than always paying the timeout.
//
// The caller MUST close stop once it no longer cares. On the coalesced path the count never
// reaches n — the other callers are parked in the singleflight — so the poller would otherwise
// spin on a 1ms sleep for the remainder of the package run, one more each -count iteration.
func allEntered(ch chan struct{}, n int, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for len(ch) < n {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return done
}

// TestClientCache_TwitterDispatchUsesTheCachedClient pins the CREATE path specifically. The
// toggle path resolving through the cache does not imply Dispatch does: Google Ads is wired on
// its toggle/metrics entry point while GoogleAdsDispatcher.Dispatch still builds inline (see
// TestClientCache_GoogleAdsDispatchBypassesTheCache). Dispatch is the burst path for X, so it is
// the one that most needs the shared pacer.
//
// It COUNTS CONSTRUCTIONS rather than comparing client identity across the call. Comparing
// identity cannot detect this regression: a Dispatch that builds inline never writes to
// d.clients at all, so the primed entry survives untouched and an identity comparison passes
// either way. A counting option is the only thing that distinguishes "reused the cached client"
// from "quietly built its own".
func TestClientCache_TwitterDispatchUsesTheCachedClient(t *testing.T) {
	repo := &syncConnReader{row: twitterCacheConn("conn-1", goodTwitterCreds, "acc1", 1)}

	// A local stub, and WithBaseURL pointing at it, so this test can never reach the real
	// X Ads origin. Without the override the client would default to
	// twitter.DefaultBaseURL. Dispatch happens to fail budget validation before any
	// request today, but relying on that would make the isolation of this test an
	// accident of an unrelated validation order: a later change to the fixture or to
	// where budget is checked would silently turn it into a live network call with the
	// client's 30s timeout.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer stub.Close()

	// twitter.Option runs once per NewClient call, so it is an exact construction counter.
	var builds atomic.Int64
	countingOpt := func(*twitter.Client) { builds.Add(1) }

	d := NewTwitterDispatcher(repo, identityEncryptor{},
		twitter.WithBaseURL(stub.URL), twitter.WithWriteDelay(0), countingOpt)

	// Prime the cache through the toggle-path resolver: one construction.
	if _, err := d.resolveTwitterClient(context.Background(), "cncf", model.ProviderTwitterAds,
		twitterCacheCampaign("acc1")); err != nil {
		t.Fatalf("priming resolve: %v", err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("priming resolve made %d clients, want 1", got)
	}

	// Dispatch will fail at the HTTP layer (no server), which is fine: the client has already
	// been resolved by then, and that is what is under test.
	_, _ = d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds,
		json.RawMessage(`{"budgetAmount":100,"startDate":"2999-01-01","endDate":"2999-01-02"}`))

	if got := builds.Load(); got != 1 {
		t.Errorf("Dispatch constructed %d clients in total, want 1 — the create path is not "+
			"going through cachedTwitterClient, so a dispatch burst gets one write pacer per "+
			"campaign instead of sharing the account's budget", got)
	}
}

// metaCacheConn is redditCacheConn for Meta. page_id is carried because Dispatch requires it;
// the toggle/metrics client this test exercises does not read it.
func metaCacheConn(id, creds, accountID string, version int64) *model.Connection {
	return &model.Connection{
		ID:                   id,
		Version:              version,
		Provider:             model.ProviderMetaAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"page_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// linkedinCacheConn is redditCacheConn for LinkedIn.
func linkedinCacheConn(id, creds, accountID string, version int64) *model.Connection {
	return &model.Connection{
		ID:                   id,
		Version:              version,
		Provider:             model.ProviderLinkedInAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"org_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// TestClientCache_MetaReusesClient pins the LFXV2-3033 wiring on the Meta toggle/metrics path:
// repeated resolves of one unchanged connection hand back the SAME *meta.Client.
//
// It asserts client IDENTITY rather than a token hit count, and the difference from the
// Reddit/Microsoft tests above is a property of the provider, not a weaker test. Meta is handed an
// already-minted bearer token and performs NO exchange at construction, so there is no token
// endpoint to count — a count-based test would read 0 with and without the cache and prove
// nothing. Identity is what the cache actually promises here.
//
// Reverting cachedMetaClient's body to a direct meta.NewClient(...) is a COMPILING change that
// leaves the rest of the suite green; this is the test that fails.
func TestClientCache_MetaReusesClient(t *testing.T) {
	repo := &syncConnReader{row: metaCacheConn("conn-1", goodMetaCreds, "act_777", 1)}
	d := NewMetaDispatcher(repo, identityEncryptor{})

	var first *meta.Client
	for i := range 5 {
		res, creds, err := d.resolveMetaCredentials(context.Background(), "cncf", model.ProviderMetaAds, d.creds.resolve)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		c := d.cachedMetaClient("cncf", model.ProviderMetaAds, res, creds)
		if i == 0 {
			first = c
			continue
		}
		if c != first {
			t.Fatalf("resolve #%d returned a NEW client for an unchanged connection: the client "+
				"cache is not being consulted on the Meta toggle/metrics path", i)
		}
	}
}

// TestClientCache_MetaRotationForcesRebuild is the invalidation contract on the Meta path: a
// client built from credential version N must not survive a bump to N+1. Without it the reuse
// test above would be satisfied by a cache that never invalidates, which would serve a revoked
// credential through a live client.
func TestClientCache_MetaRotationForcesRebuild(t *testing.T) {
	repo := &syncConnReader{row: metaCacheConn("conn-1", goodMetaCreds, "act_777", 1)}
	d := NewMetaDispatcher(repo, identityEncryptor{})

	res, creds, err := d.resolveMetaCredentials(context.Background(), "cncf", model.ProviderMetaAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	first := d.cachedMetaClient("cncf", model.ProviderMetaAds, res, creds)

	repo.row = metaCacheConn("conn-1", `{"AccessToken":"rotated"}`, "act_777", 2)
	res2, creds2, err := d.resolveMetaCredentials(context.Background(), "cncf", model.ProviderMetaAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("resolve after rotation: %v", err)
	}
	if c := d.cachedMetaClient("cncf", model.ProviderMetaAds, res2, creds2); c == first {
		t.Error("a rotated credential (version 1 -> 2) was served through the client built from " +
			"the OLD one: the cache entry is not validated against the row version")
	}
}

// TestClientCache_LinkedInReusesClient is TestClientCache_MetaReusesClient on the LinkedIn
// toggle/metrics path, and asserts identity for the same reason: LinkedIn is handed an
// already-minted bearer token and performs no exchange at construction on this fixture.
func TestClientCache_LinkedInReusesClient(t *testing.T) {
	repo := &syncConnReader{row: linkedinCacheConn("conn-1", goodLinkedInCreds, "123456789", 1)}
	d := NewLinkedInDispatcher(repo, identityEncryptor{})

	var first *linkedin.Client
	for i := range 5 {
		res, creds, err := d.resolveLinkedInCredentials(context.Background(), "cncf", model.ProviderLinkedInAds, d.creds.resolve)
		if err != nil {
			t.Fatalf("resolve #%d: %v", i, err)
		}
		c := d.cachedLinkedInClient("cncf", model.ProviderLinkedInAds, res, creds, res.accountID)
		if i == 0 {
			first = c
			continue
		}
		if c != first {
			t.Fatalf("resolve #%d returned a NEW client for an unchanged connection: the client "+
				"cache is not being consulted on the LinkedIn toggle/metrics path", i)
		}
	}
}

// TestClientCache_LinkedInRotationForcesRebuild is the Meta rotation test on the LinkedIn path.
func TestClientCache_LinkedInRotationForcesRebuild(t *testing.T) {
	repo := &syncConnReader{row: linkedinCacheConn("conn-1", goodLinkedInCreds, "123456789", 1)}
	d := NewLinkedInDispatcher(repo, identityEncryptor{})

	res, creds, err := d.resolveLinkedInCredentials(context.Background(), "cncf", model.ProviderLinkedInAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	first := d.cachedLinkedInClient("cncf", model.ProviderLinkedInAds, res, creds, res.accountID)

	repo.row = linkedinCacheConn("conn-1", `{"AccessToken":"rotated"}`, "123456789", 2)
	res2, creds2, err := d.resolveLinkedInCredentials(context.Background(), "cncf", model.ProviderLinkedInAds, d.creds.resolve)
	if err != nil {
		t.Fatalf("resolve after rotation: %v", err)
	}
	if c := d.cachedLinkedInClient("cncf", model.ProviderLinkedInAds, res2, creds2, res2.accountID); c == first {
		t.Error("a rotated credential (version 1 -> 2) was served through the client built from " +
			"the OLD one: the cache entry is not validated against the row version")
	}
}

// TestClientCache_MetaEntryPointsUseTheCachedClient pins the WIRING, which the identity tests
// above cannot reach.
//
// They call cachedMetaClient directly, so they keep passing if ToggleStatus and ReadMetrics
// construct clients inline and never consult the helper — the cache would be fully tested and
// simply unused by the paths it exists for. That is not hypothetical: reverting both entry points
// to a direct meta.NewClient(...) is a COMPILING change that leaves the identity tests green.
//
// What binds it is CONSTRUCTION COUNT across calls through the real entry points. A meta.Option
// runs once per NewClient, so counting invocations counts constructions: cached construction
// builds once for an unchanged connection, inline construction builds per call. Counting a token
// endpoint is not available here — Meta mints no token — which is exactly why this is the
// instrument.
func TestClientCache_MetaEntryPointsUseTheCachedClient(t *testing.T) {
	var builds atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		if strings.Contains(r.URL.Path, "insights") {
			_, _ = io.WriteString(w, `{"data":[{"impressions":"1","clicks":"1","spend":"1.00"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	t.Cleanup(srv.Close)

	countingOpt := func(_ *meta.Client) { builds.Add(1) }
	repo := &syncConnReader{row: metaCacheConn("conn-1", goodMetaCreds, "act_777", 1)}
	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL), countingOpt)

	camp := metaToggleCampaign("777", "888")
	for i := range 2 {
		if err := d.ToggleStatus(context.Background(), "cncf", model.ProviderMetaAds, camp, model.CampaignRunPaused); err != nil {
			t.Fatalf("ToggleStatus #%d: %v", i, err)
		}
	}
	if _, err := d.ReadMetrics(context.Background(), "cncf", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days); err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}

	// Three calls through two entry points on ONE unchanged connection: they share a cache key,
	// so exactly one client is built. Any inline construction pushes this to 2 or 3.
	if n := builds.Load(); n != 1 {
		t.Errorf("built %d meta clients across 2 toggles + 1 metrics read of one unchanged "+
			"connection, want 1 — an entry point is constructing its client inline instead of "+
			"going through cachedMetaClient, so the client cache does nothing for these paths", n)
	}
}

// TestClientCache_LinkedInEntryPointsUseTheCachedClient is the Meta wiring test on the LinkedIn
// toggle/metrics paths, and it matters more there: a refresh-capable LinkedIn connection performs
// an OAuth exchange on the FIRST request of every new client, so an unwired entry point re-mints
// a token per request rather than merely reallocating.
func TestClientCache_LinkedInEntryPointsUseTheCachedClient(t *testing.T) {
	var builds atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "adAnalytics"):
			_, _ = io.WriteString(w, `{"elements":[]}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"elements":[]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	countingOpt := func(_ *linkedin.Client) { builds.Add(1) }
	repo := &syncConnReader{row: linkedinCacheConn("conn-1", goodLinkedInCreds, "123456789", 1)}
	d := NewLinkedInDispatcher(repo, identityEncryptor{}, linkedin.WithBaseURL(srv.URL), countingOpt)

	camp := &model.Campaign{Platform: model.ProviderLinkedInAds, PlatformCampaignID: "urn:li:sponsoredCampaign:777"}
	// Errors are tolerated: this asserts how many clients were BUILT, not that each call
	// succeeded against the stub. A failure after construction still counts its construction,
	// and gating on success would make the test fragile to unrelated stub shape changes.
	for range 2 {
		_ = d.ToggleStatus(context.Background(), "cncf", model.ProviderLinkedInAds, camp, model.CampaignRunPaused)
	}
	_, _ = d.ReadMetrics(context.Background(), "cncf", model.ProviderLinkedInAds, camp, model.MetricsWindowLast30Days)

	if n := builds.Load(); n != 1 {
		t.Errorf("built %d linkedin clients across 2 toggles + 1 metrics read of one unchanged "+
			"connection, want 1 — an entry point is constructing its client inline instead of "+
			"going through cachedLinkedInClient, so a refresh-capable connection re-mints an "+
			"OAuth token per request", n)
	}
}

// TestClientCache_MetaColdKeyConcurrentBuildsAreCoalesced is the Reddit cold-key burst test on the
// Meta path, and it carries the load the sequential tests cannot: on a COLD key every caller
// misses at once, so without coalescing each builds its own client and the cache does nothing for
// the burst it exists for.
//
// It is also the -race exercise for SHARING one meta.Client across concurrent callers, which is
// the property the wiring rests on. Meta's client is immutable once built (every field written at
// construction, no token cache, no in-flight handle), and this is what tests that claim rather
// than asserting it in a comment.
func TestClientCache_MetaColdKeyConcurrentBuildsAreCoalesced(t *testing.T) {
	const callers = 16
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"impressions":"1","clicks":"1","spend":"1.00"}]}`)
	}))
	t.Cleanup(srv.Close)

	// NO barrier here, unlike the Reddit/Microsoft cold-key tests. Those resolve once per
	// goroutine, so a 16-party barrier is what aligns them; this test resolves ONCE up front
	// (see below) and would deadlock waiting for 15 arrivals that never come. The start channel
	// below is what aligns the callers instead, and it aligns them on buildOnce — which is the
	// window this test is actually about.
	repo := &syncConnReader{row: metaCacheConn("conn-1", goodMetaCreds, "act_777", 1)}
	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []*meta.Client
	)
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}

	// Resolve ONCE up front and share the result. This is what puts every caller inside
	// buildOnce's cache-miss window together, and it is load-bearing rather than a shortcut:
	// resolving per goroutine coalesces at decryptOnce FIRST, so the leader completes its build
	// and put before the followers reach buildOnce, which then serves them a WARM entry. That
	// version of this test passed with singleflight removed — it verified the credential cache,
	// not this one. Verified by mutation: with the shared resolve, deleting c.group.Do makes
	// this fail.
	res, creds, rerr := d.resolveMetaCredentials(context.Background(), "cncf", model.ProviderMetaAds, d.creds.resolve)
	if rerr != nil {
		t.Fatalf("pre-resolve: %v", rerr)
	}
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c := d.cachedMetaClient("cncf", model.ProviderMetaAds, res, creds)
			// The lock covers ONLY the append, never the traffic below: holding it across the
			// call would serialize all 16 callers and -race would observe no overlap.
			mu.Lock()
			got = append(got, c)
			mu.Unlock()

			// Drive real traffic through the SHARED instance, outside the bookkeeping lock.
			_, _ = d.ReadMetrics(context.Background(), "cncf", model.ProviderMetaAds, camp, model.MetricsWindowLast30Days)
		}()
	}
	close(start)
	wg.Wait()

	if len(got) != callers {
		t.Fatalf("got %d clients, want %d", len(got), callers)
	}
	for i, c := range got {
		if c != got[0] {
			t.Fatalf("caller %d received a different client instance: construction is not "+
				"coalesced, so a cold key under a burst builds one client per caller, exactly "+
				"as if there were no cache", i)
		}
	}
}

// TestClientCache_LinkedInColdKeyConcurrentBuildsAreCoalesced is the Meta cold-key burst test on
// the LinkedIn path: it pins that a cold key under a burst yields ONE shared client, and a
// coalescing failure costs more here than for Meta, because a refresh-capable connection mints a
// token on each new client's first request.
//
// SCOPE, stated precisely because the obvious reading is wrong: this does NOT exercise the
// mutable token state. goodLinkedInCreds is bearer-only, so Credentials.CanRefresh() is false and
// the client never writes c.accessToken/c.tokenExpiry/c.inflight — -race here observes concurrent
// READS of an effectively immutable client, which is real but weaker than it looks. Driving the
// write path from this package is not possible today: the token endpoint is reachable only via an
// unexported option, so a refresh-capable fixture here would still never exchange. The mutex
// discipline that makes SHARING safe is covered where it can be: internal/platform/linkedin's own
// token tests (single-flight coalescing, rotation, invalidation) run against a real token server.
// Do not read this test as that evidence.
func TestClientCache_LinkedInColdKeyConcurrentBuildsAreCoalesced(t *testing.T) {
	const callers = 16
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "adAnalytics"):
			_, _ = io.WriteString(w, `{"elements":[]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	// No barrier, for the reason given on the Meta cold-key test: one shared resolve cannot
	// satisfy a 16-party barrier, and the start channel aligns the callers on buildOnce instead.
	repo := &syncConnReader{row: linkedinCacheConn("conn-1", goodLinkedInCreds, "123456789", 1)}
	d := NewLinkedInDispatcher(repo, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []*linkedin.Client
	)
	camp := &model.Campaign{Platform: model.ProviderLinkedInAds, PlatformCampaignID: "urn:li:sponsoredCampaign:777"}

	// Resolved ONCE and shared, for the reason given on the Meta test above: a per-goroutine
	// resolve coalesces at decryptOnce first and hands the followers a warm client cache, which
	// made this pass with coalescing removed.
	res, creds, rerr := d.resolveLinkedInCredentials(context.Background(), "cncf", model.ProviderLinkedInAds, d.creds.resolve)
	if rerr != nil {
		t.Fatalf("pre-resolve: %v", rerr)
	}
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c := d.cachedLinkedInClient("cncf", model.ProviderLinkedInAds, res, creds, res.accountID)
			mu.Lock()
			got = append(got, c)
			mu.Unlock()

			// Concurrent traffic through the SHARED instance, outside the bookkeeping lock, so
			// -race observes real overlap rather than construction alone. Per the SCOPE note
			// above, this fixture makes that overlapping READS of an effectively immutable
			// client — the token-state writes are covered in internal/platform/linkedin.
			_, _ = d.ReadMetrics(context.Background(), "cncf", model.ProviderLinkedInAds, camp, model.MetricsWindowLast30Days)
		}()
	}
	close(start)
	wg.Wait()

	if len(got) != callers {
		t.Fatalf("got %d clients, want %d", len(got), callers)
	}
	for i, c := range got {
		if c != got[0] {
			t.Fatalf("caller %d received a different client instance: construction is not "+
				"coalesced, so a cold key under a burst builds one client per caller and a "+
				"refresh-capable connection mints one OAuth token each", i)
		}
	}
}
