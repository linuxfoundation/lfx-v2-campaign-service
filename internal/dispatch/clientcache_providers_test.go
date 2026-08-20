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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// This file covers the LFXV2-3033 extension of clientCache to Reddit and Microsoft. Google Ads
// already had its own coverage in credcache_test.go; these are the two providers this change
// wires, and each gets the same three properties the Google Ads client cache is held to:
//
//  1. a warm key REUSES the client, so the OAuth token is minted once rather than per call;
//  2. a credential change (rotation, which bumps the version, and reconnect, which restarts it
//     at 1 on a new row) FORCES reconstruction — the invalidation contract;
//  3. a cold key under a concurrent burst builds ONE client, and the shared instance is safe
//     under -race.
//
// Reddit and Microsoft need their own tests rather than inheriting the Google Ads ones because
// the safety argument is per-client: each caches its access token on the instance behind its own
// mutex, and a client that stashed per-call state on the receiver would be unsafe to share no
// matter how correct the cache is. See the struct comments on RedditDispatcher.clients and
// MicrosoftDispatcher.clients.

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
// pointing ALL THREE of the client's origins at the stub. It exists so the reporting override
// cannot be forgotten in one test and silently reintroduce the live call: microsoft.Client splits
// its API across three hosts (Campaign Management, Customer Management and Reporting), and an
// un-overridden origin falls back to the production default rather than failing loudly.
func newMicrosoftCacheDispatcher(repo connReader, srv *httptest.Server) *MicrosoftDispatcher {
	return NewMicrosoftDispatcher(repo, identityEncryptor{},
		microsoft.WithTokenURL(srv.URL+"/token"),
		microsoft.WithBaseURL(srv.URL),
		microsoft.WithCustomerBaseURL(srv.URL),
		microsoft.WithReportingBaseURL(srv.URL))
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
	d := NewMicrosoftDispatcher(repo, identityEncryptor{},
		microsoft.WithTokenURL(srv.URL+"/token"),
		microsoft.WithBaseURL(srv.URL),
		microsoft.WithCustomerBaseURL(srv.URL),
		microsoft.WithReportingBaseURL(srv.URL),
		microsoft.WithHTTPClient(hc))

	c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds)
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
		c, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds)
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

	c1, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Drive a call so this client actually mints a token. A rebuild is only observable as a
	// SECOND token exchange if the first client ever authenticated.
	redditProbe(t, c1)

	// Rotate the credential on the SAME row: every mutating statement in ConnectionRepo bumps
	// version, so the new credential arrives as version 2.
	repo.row = redditCacheConn("conn-1", `{"ClientID":"cid2","ClientSecret":"sec2","RefreshToken":"rt2"}`, "t2_acct", 2)

	c2, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds)
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

	c3, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds)
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
			c, err := d.resolveRedditClient(context.Background(), "cncf", model.ProviderRedditAds)
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
// multi-customer discovery, so it was the one where a per-call CustomerID stashed on the receiver
// would have made a shared instance serve one caller against another caller's customer. It does
// not do that (the customer id travels as a per-call argument), which is what makes the sharing
// below safe — see the MicrosoftDispatcher.clients comment.
func TestClientCache_MicrosoftReusesClientAndToken(t *testing.T) {
	srv, tokenHits := tokenCountingServer(t, `{"value":[]}`)

	repo := &syncConnReader{row: microsoftCacheConn("conn-1", goodMicrosoftCreds, "111111", 1)}
	d := newMicrosoftCacheDispatcher(repo, srv)

	var first *microsoft.Client
	for i := range 5 {
		c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds)
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

	c1, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Drive a call so this client actually mints a token. A rebuild is only observable as a
	// SECOND token exchange if the first client ever authenticated.
	microsoftProbe(t, c1)

	rotated := `{"ClientID":"cid2","ClientSecret":"csec2","DeveloperToken":"dev2","RefreshToken":"rt2"}`
	repo.row = microsoftCacheConn("conn-1", rotated, "111111", 2)

	c2, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds)
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

	c3, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds)
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
			c, err := d.resolveMicrosoftClient(context.Background(), "cncf", model.ProviderMicrosoftAds)
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
