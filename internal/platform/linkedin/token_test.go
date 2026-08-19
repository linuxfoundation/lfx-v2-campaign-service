// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// linkedInSampleTokenResponse is LinkedIn's PUBLISHED sample response for the
// refresh exchange, copied from
// https://learn.microsoft.com/en-us/linkedin/shared/authentication/programmatic-refresh-tokens
// (Step 2, "A successful request returns a new access token"). It is used verbatim
// rather than hand-rolled so the decoder is tested against the vendor's documented
// shape — including refresh_token_expires_in, which a fixture written from our own
// assumptions would likely have omitted.
const linkedInSampleTokenResponse = `{
  "access_token": "BBBB2kXITHELmWblJigbHEuoFdfRhOwGA0QNnumBI8XOVSs0HtOHEU-wvaKrkMLfxxaB1O4poRg2svCWWgwhebQhqrETYlLikJJMgRAvH1ostjXd3DP3Btw",
  "expires_in": 86400,
  "refresh_token": "AQWAft_WjYZKwuWXLC5hQlghgTam-tuT8CvFej9-XxGyqeER_7jTr8HmjiGjqil13i7gMFjyDxh1g7C_G1gyTZmfcD0Bo2oEHofNAkr",
  "refresh_token_expires_in": 439200,
  "scope":"r_basicprofile"
}`

const sampleAccessToken = "BBBB2kXITHELmWblJigbHEuoFdfRhOwGA0QNnumBI8XOVSs0HtOHEU-wvaKrkMLfxxaB1O4poRg2svCWWgwhebQhqrETYlLikJJMgRAvH1ostjXd3DP3Btw"

// refreshableCreds is a connection that CAN refresh, with an already-expired access
// token so any token read must go through the exchange.
func refreshableCreds() Credentials {
	return Credentials{
		AccessToken:          "stale-access-token",
		AccessTokenExpiresAt: time.Now().Add(-time.Hour),
		RefreshToken:         "stored-refresh-token",
		ClientID:             "client-id",
		ClientSecret:         "client-secret",
		ConnectionName:       "LF LinkedIn",
	}
}

// tokenServer serves LinkedIn's sample response and counts exchanges.
func tokenServer(t *testing.T, count *atomic.Int32, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(linkedInSampleTokenResponse))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAccessTokenValueSingleFlight is the single-flight property test: N concurrent
// callers must produce exactly ONE token exchange. It asserts the SERVER-SIDE call
// COUNT, not merely that a token came back — a per-caller refresh would still return
// a valid token to everyone and pass a weaker assertion while hammering the token
// endpoint and risking rate limits.
func TestAccessTokenValueSingleFlight(t *testing.T) {
	var exchanges atomic.Int32
	// A delay guarantees the followers arrive while the leader's fetch is in flight;
	// without it the calls could serialize and each legitimately find an empty cache.
	srv := tokenServer(t, &exchanges, 50*time.Millisecond)

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))

	const callers = 25
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = c.accessTokenValue(context.Background())
		}()
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error: %v", i, errs[i])
		}
		if tokens[i] != sampleAccessToken {
			t.Fatalf("caller %d: got token %q, want the refreshed token", i, tokens[i])
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want exactly 1 (concurrent callers must coalesce)", got)
	}
}

// TestAccessTokenValueCachesRefreshedToken proves the refreshed token is cached: a
// second sequential call must not re-exchange.
func TestAccessTokenValueCachesRefreshedToken(t *testing.T) {
	var exchanges atomic.Int32
	srv := tokenServer(t, &exchanges, 0)
	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))

	for i := range 3 {
		tok, err := c.accessTokenValue(context.Background())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if tok != sampleAccessToken {
			t.Fatalf("call %d: got %q", i, tok)
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1 (a cached, unexpired token must be reused)", got)
	}
}

// TestAccessTokenValueSendsDocumentedExchange pins the exchange to LinkedIn's
// published contract: POST, form-encoded, with grant_type/refresh_token/client_id/
// client_secret.
func TestAccessTokenValueSendsDocumentedExchange(t *testing.T) {
	// httptest runs each handler in its own goroutine, so the captures need a
	// happens-before edge to the assertions. Mirrors metrics_test.go.
	var mu sync.Mutex
	var (
		gotMethod, gotContentType string
		gotForm                   url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotForm = r.PostForm
		mu.Unlock()
		_, _ = w.Write([]byte(linkedInSampleTokenResponse))
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	method, contentType, form := gotMethod, gotContentType, gotForm
	mu.Unlock()

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want x-www-form-urlencoded", contentType)
	}
	for k, want := range map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "stored-refresh-token",
		"client_id":     "client-id",
		"client_secret": "client-secret",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("form[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestAccessTokenValueAdoptsRotatedRefreshToken proves the refresh_token returned by
// the exchange is adopted for the NEXT exchange. LinkedIn returns the refresh token
// on every exchange; ignoring it would keep using a superseded value.
func TestAccessTokenValueAdoptsRotatedRefreshToken(t *testing.T) {
	// Guarded for the same reason as the exchange test above: the append runs on the
	// handler goroutine and the assertions read it from the test goroutine.
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		seen = append(seen, r.PostForm.Get("refresh_token"))
		mu.Unlock()
		// expires_in 0 so the access token is never cached and a second call re-exchanges.
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":-1,"refresh_token":"rotated-refresh-token"}`))
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	c.now = func() time.Time { return time.Now() }
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Force the cache to be stale so a second exchange happens.
	c.tokenMu.Lock()
	c.tokenExpiry = time.Now().Add(-time.Hour)
	c.tokenMu.Unlock()
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("second: %v", err)
	}

	mu.Lock()
	exchanged := append([]string(nil), seen...)
	mu.Unlock()

	if len(exchanged) != 2 {
		t.Fatalf("exchanges = %d, want 2", len(exchanged))
	}
	if exchanged[0] != "stored-refresh-token" {
		t.Errorf("first exchange used %q, want the stored token", exchanged[0])
	}
	if exchanged[1] != "rotated-refresh-token" {
		t.Errorf("second exchange used %q, want the rotated token adopted from the response", exchanged[1])
	}
}

// TestAccessTokenValueRejectedRefreshIsActionable proves a rejected refresh token
// fails CLOSED with ErrCredentialsExpired naming the connection — not a 500, and
// never a fallback to the stale token.
//
// The body carries `invalid_grant`, the RFC 6749 §5.2 code that actually means the refresh
// token is expired or revoked. It previously carried `invalid_request` with an
// expired-sounding description — but the CODE is what classifies, the description is never
// read, and invalid_request means a malformed REQUEST. The fixture and the code agreed only
// because the split was binary; under a correct classification that body is an application
// fault and this test would have been asserting the wrong remedy.
func TestAccessTokenValueRejectedRefreshIsActionable(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token is invalid, expired or revoked"}`))
		}))

		c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
		tok, err := c.accessTokenValue(context.Background())
		srv.Close()

		if !errors.Is(err, ErrCredentialsExpired) {
			t.Fatalf("status %d: error = %v, want ErrCredentialsExpired", status, err)
		}
		if tok != "" {
			t.Fatalf("status %d: got token %q, want empty (must fail closed, never reuse the stale token)", status, tok)
		}
		if !strings.Contains(err.Error(), "LF LinkedIn") {
			t.Errorf("status %d: error %q must name the connection so an operator knows what to reconnect", status, err)
		}
	}
}

// TestRefreshErrorNeverLeaksCredentials proves no error surfaced from the token
// exchange echoes the response body or any credential. These errors are persisted
// into a campaign's Steps, so a leak would be durable.
func TestRefreshErrorNeverLeaksCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// A hostile/naive upstream body reflecting the credentials we sent.
		_, _ = w.Write([]byte(`{"error":"bad","client_secret":"client-secret","refresh_token":"stored-refresh-token"}`))
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"client-secret", "stored-refresh-token", "stale-access-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks credential material %q", err, secret)
		}
	}
}

// TestBearerOnlyConnectionUnchanged proves a connection with NO refresh material
// still works exactly as before — LinkedIn issues refresh tokens only to approved
// MDP partners, so this is the common case and must not regress.
func TestBearerOnlyConnectionUnchanged(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "plain-bearer"}, RuntimeConfig{},
		withTokenURL("http://127.0.0.1:1")) // any exchange attempt would fail loudly

	tok, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("bearer-only connection must keep working: %v", err)
	}
	if tok != "plain-bearer" {
		t.Fatalf("got %q, want the injected token unchanged", tok)
	}
}

// TestBearerOnlyKnownExpiredFailsClosed proves that when the access token's expiry
// is KNOWN and past and there is no refresh token, the client fails closed with the
// actionable error rather than sending a request guaranteed to 401.
func TestBearerOnlyKnownExpiredFailsClosed(t *testing.T) {
	c := NewClient(Credentials{
		AccessToken:          "expired-bearer",
		AccessTokenExpiresAt: time.Now().Add(-time.Minute),
		ConnectionName:       "LF LinkedIn",
	}, RuntimeConfig{})

	tok, err := c.accessTokenValue(context.Background())
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("error = %v, want ErrCredentialsExpired", err)
	}
	if tok != "" {
		t.Fatalf("got %q, want empty (fail closed)", tok)
	}
}

// TestUnknownExpiryIsNotTreatedAsExpired guards the boundary the repo has been bitten
// by: a ZERO AccessTokenExpiresAt means "unknown", and absence must not be read as
// evidence of expiry — that would break every existing connection, none of which
// store an expiry today.
func TestUnknownExpiryIsNotTreatedAsExpired(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "no-expiry-known"}, RuntimeConfig{})
	tok, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("unknown expiry must not fail: %v", err)
	}
	if tok != "no-expiry-known" {
		t.Fatalf("got %q", tok)
	}
}

// TestExpiredRefreshTokenFailsWithoutNetworkCall proves a refresh token known to be
// past its own deadline fails closed WITHOUT spending a round-trip: LinkedIn does not
// reset a refresh token's TTL on use, so this deadline is a hard stop.
func TestExpiredRefreshTokenFailsWithoutNetworkCall(t *testing.T) {
	var exchanges atomic.Int32
	srv := tokenServer(t, &exchanges, 0)

	creds := refreshableCreds()
	creds.RefreshTokenExpiresAt = time.Now().Add(-time.Hour)
	c := NewClient(creds, RuntimeConfig{}, withTokenURL(srv.URL))

	_, err := c.accessTokenValue(context.Background())
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("error = %v, want ErrCredentialsExpired", err)
	}
	if got := exchanges.Load(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0 (an expired refresh token can never mint anything)", got)
	}
}

// TestValidAccessTokenSkipsRefresh proves a still-valid injected access token is used
// directly, with no exchange, even when refresh material is present.
func TestValidAccessTokenSkipsRefresh(t *testing.T) {
	var exchanges atomic.Int32
	srv := tokenServer(t, &exchanges, 0)

	creds := refreshableCreds()
	creds.AccessTokenExpiresAt = time.Now().Add(24 * time.Hour)
	c := NewClient(creds, RuntimeConfig{}, withTokenURL(srv.URL))

	tok, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "stale-access-token" {
		t.Fatalf("got %q, want the injected still-valid token", tok)
	}
	if got := exchanges.Load(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
}

// TestEmptyAccessTokenInResponseFailsClosed proves a malformed success does not
// silently proceed with the stale token.
func TestEmptyAccessTokenInResponseFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"","expires_in":86400}`))
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	tok, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error on an empty access_token")
	}
	if tok != "" {
		t.Fatalf("got %q, want empty", tok)
	}
}

// TestIsTokenExpiryResponse pins the classification decision: 65602 is ONE signal,
// not the complete one. LinkedIn documents expired, revoked and invalid tokens as
// distinct 401s and publishes no subcode for the latter two, so a 401 without 65602
// must still be treated as an auth-credential failure.
func TestIsTokenExpiryResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"expired with 65602", http.StatusUnauthorized, `{"serviceErrorCode":65602,"message":"The token used in the request has expired","status":401}`, true},
		{"revoked without a subcode", http.StatusUnauthorized, `{"message":"The token has been revoked","status":401}`, true},
		{"invalid token, unparseable body", http.StatusUnauthorized, `not json`, true},
		{"403 is not an expiry", http.StatusForbidden, `{"status":403}`, false},
		{"429 is not an expiry", http.StatusTooManyRequests, `{"status":429}`, false},
		{"200 is not an expiry", http.StatusOK, `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTokenExpiryResponse(tc.status, tc.body); got != tc.want {
				t.Fatalf("isTokenExpiryResponse(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestConcurrentRefreshAndCredentialReadsAreRaceFree drives the exact interleaving
// that a per-attempt token resolve creates in production: many goroutines resolving
// tokens while exchanges rotate the refresh token, alongside the credential-shape
// preflight that reads c.creds WITHOUT the token lock. Run under -race, this fails if
// rotation is ever written back into the injected c.creds instead of the dedicated
// mutable fields.
func TestConcurrentRefreshAndCredentialReadsAreRaceFree(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		// expires_in -1 so nothing caches and every call re-exchanges, maximising overlap.
		_, _ = fmt.Fprintf(w, `{"access_token":"a%d","expires_in":-1,"refresh_token":"rot-%d","refresh_token_expires_in":600}`, i, i)
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))

	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.accessTokenValue(context.Background())
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Reads c.creds without tokenMu — the other half of the race.
			_ = c.validateCredentialShape()
		}()
	}
	wg.Wait()
}

// TestRotatedRefreshTokenDoesNotMutateInjectedCredentials pins WHERE rotation is
// stored. The injected Credentials value must be treated as immutable: mutating it is
// what created the race, and a caller may legitimately reuse the same struct.
func TestRotatedRefreshTokenDoesNotMutateInjectedCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":86400,"refresh_token":"rotated-value"}`))
	}))
	defer srv.Close()

	creds := refreshableCreds()
	c := NewClient(creds, RuntimeConfig{}, withTokenURL(srv.URL))
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c.creds.RefreshToken != "stored-refresh-token" {
		t.Errorf("injected creds were mutated: RefreshToken = %q, want the original", c.creds.RefreshToken)
	}
	c.tokenMu.Lock()
	got := c.refreshToken
	c.tokenMu.Unlock()
	if got != "rotated-value" {
		t.Errorf("rotation not adopted into the mutable field: got %q", got)
	}
}

// TestMetricsPathResolvesTokenThroughRefresh pins that the Ad Analytics read path —
// the very call that surfaced the production 500 on 2026-08-14 — fails closed on an
// unrefreshable credential instead of sending a known-expired bearer token. It read
// c.creds.AccessToken directly before this change, bypassing refresh entirely.
func TestMetricsPathResolvesTokenThroughRefresh(t *testing.T) {
	var apiCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		_, _ = w.Write([]byte(`{"elements":[]}`))
	}))
	defer api.Close()

	// A bearer-only connection whose token is KNOWN expired: nothing can refresh it.
	c := NewClient(Credentials{
		AccessToken:          "expired-bearer",
		AccessTokenExpiresAt: time.Now().Add(-time.Hour),
		ConnectionName:       "LF LinkedIn",
	}, RuntimeConfig{}, WithBaseURL(api.URL))

	_, _, _, err := c.doAdAnalyticsAttempt(context.Background(), api.URL+"/adAnalytics")
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("error = %v, want ErrCredentialsExpired", err)
	}
	if got := apiCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 (must fail closed before sending an expired token)", got)
	}
}

// TestMidFlight401SurfacesActionableExpiryAndInvalidatesCache pins the 401 path.
// LinkedIn "reserves the right to revoke Refresh Tokens or Access Tokens at any time",
// so a token valid when the request was built can be rejected mid-flight — no expiry
// timestamp predicts that. The 401 must (a) surface as ErrCredentialsExpired naming
// the connection rather than a bare "LinkedIn API ... -> 401", and (b) invalidate the
// cached token so the next call re-exchanges instead of replaying a dead one.
func TestMidFlight401SurfacesActionableExpiryAndInvalidatesCache(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"serviceErrorCode":65602,"message":"The token used in the request has expired","status":401}`))
	}))
	defer api.Close()

	c := NewClient(Credentials{AccessToken: "live-token", ConnectionName: "LF LinkedIn"},
		RuntimeConfig{}, WithBaseURL(api.URL))

	// Seed the cache as a refresh would, so invalidation is observable.
	c.tokenMu.Lock()
	c.accessToken = "cached-token"
	c.tokenExpiry = time.Now().Add(time.Hour)
	c.tokenMu.Unlock()

	_, err := c.doRequest(context.Background(), http.MethodGet, "/adAccounts", nil, nil, nil)
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("error = %v, want ErrCredentialsExpired", err)
	}
	if !strings.Contains(err.Error(), "LF LinkedIn") {
		t.Errorf("error %q must name the connection", err)
	}

	c.tokenMu.Lock()
	cached := c.accessToken
	c.tokenMu.Unlock()
	if cached != "" {
		t.Errorf("cached access token = %q, want it invalidated so the next call re-exchanges", cached)
	}
}

// TestNon401ErrorsAreNotTreatedAsExpiry guards the other side: a 403 or 429 must keep
// its ordinary apiError classification. Turning every failure into "reconnect your
// connection" would send operators to fix a credential that is perfectly valid.
func TestNon401ErrorsAreNotTreatedAsExpiry(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		}))

		c := NewClient(Credentials{AccessToken: "live-token", ConnectionName: "LF LinkedIn"},
			RuntimeConfig{}, WithBaseURL(api.URL))
		_, err := c.doRequest(context.Background(), http.MethodGet, "/adAccounts", nil, nil, nil)
		api.Close()

		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if errors.Is(err, ErrCredentialsExpired) {
			t.Errorf("status %d: must NOT be classified as a credential expiry", status)
		}
	}
}

// TestValidAccessTokenWinsOverExpiredRefreshToken pins the ORDER of the two checks.
// Refresh material is additive to a working bearer token: a connection whose access
// token is still valid must keep working even if its refresh token has aged out.
// Checking the refresh deadline first would mean that adding a refresh token to a
// working connection eventually BREAKS it — a strictly worse outcome than not having
// added one. Nothing here should reach the network.
func TestValidAccessTokenWinsOverExpiredRefreshToken(t *testing.T) {
	var exchanges atomic.Int32
	srv := tokenServer(t, &exchanges, 0)

	creds := refreshableCreds()
	creds.AccessTokenExpiresAt = time.Now().Add(24 * time.Hour) // still good
	creds.RefreshTokenExpiresAt = time.Now().Add(-time.Hour)    // long dead
	c := NewClient(creds, RuntimeConfig{}, withTokenURL(srv.URL))

	tok, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("a valid access token must be usable despite a dead refresh token: %v", err)
	}
	if tok != "stale-access-token" {
		t.Fatalf("got %q, want the injected still-valid access token", tok)
	}
	if got := exchanges.Load(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
}

// leakyRoundTripper models the vector the response-body leak test does NOT cover:
// WithHTTPClient accepts an arbitrary RoundTripper, and a naive or hostile one can
// echo the REQUEST body — which on the token exchange carries client_secret and
// refresh_token — into its error text. http.Client.Do then wraps that in a *url.Error,
// so peeling one layer is not enough.
type leakyRoundTripper struct{}

func (leakyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	return nil, fmt.Errorf("upstream rejected request %q", string(body))
}

// TestTokenTransportErrorNeverLeaksTheRequestBody walks EVERY layer of the returned
// error chain, not just the outermost Error(): a credential-bearing wrapper left in
// the chain is reachable by any structured logger or middleware that renders it, and
// these errors are persisted into a campaign's Steps, so a leak would be durable.
func TestTokenTransportErrorNeverLeaksTheRequestBody(t *testing.T) {
	c := NewClient(refreshableCreds(), RuntimeConfig{},
		withTokenURL("https://example.invalid/oauth/v2/accessToken"),
		WithHTTPClient(&http.Client{Transport: leakyRoundTripper{}}))

	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}

	secrets := []string{"client-secret", "stored-refresh-token", "client_secret", "refresh_token"}
	for layer := err; layer != nil; layer = errors.Unwrap(layer) {
		for _, s := range secrets {
			if strings.Contains(layer.Error(), s) {
				t.Errorf("error layer %T renders credential material %q: %v", layer, s, layer)
			}
		}
	}
}

// leakyBodyRoundTripper models the third vector, which neither the response-body test nor
// the transport-error test covers: the RoundTripper returns a RESPONSE whose Body.Read
// fails with credential-bearing text. WithHTTPClient makes the response as caller-controlled
// as the transport error, and fetchToken reads that body before it classifies the status.
type leakyBodyRoundTripper struct{}

func (leakyBodyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(&failingReader{msg: "read failed for request " + string(body)}),
	}, nil
}

// failingReader fails every Read with an error carrying the supplied text.
type failingReader struct{ msg string }

func (f *failingReader) Read([]byte) (int, error) { return 0, errors.New(f.msg) }

// TestTokenBodyReadErrorNeverLeaksTheRequestBody walks every layer of the chain, like the
// transport-error test. Wrapping the raw body-read cause with %w bypassed the redaction the
// transport arm establishes, and these errors are persisted into a campaign's Steps.
func TestTokenBodyReadErrorNeverLeaksTheRequestBody(t *testing.T) {
	c := NewClient(refreshableCreds(), RuntimeConfig{},
		withTokenURL("https://example.invalid/oauth/v2/accessToken"),
		WithHTTPClient(&http.Client{Transport: leakyBodyRoundTripper{}}))

	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("a failing body read must not read as a successful token exchange")
	}

	secrets := []string{"client-secret", "stored-refresh-token", "client_secret", "refresh_token"}
	for layer := err; layer != nil; layer = errors.Unwrap(layer) {
		for _, s := range secrets {
			if strings.Contains(layer.Error(), s) {
				t.Errorf("error layer %T renders credential material %q: %v", layer, s, layer)
			}
		}
	}
}

// TestTokenDecodeErrorNeverEchoesTheResponseBody covers the arm the other leak tests could
// not reach. TestRefreshErrorNeverLeaksCredentials serves 400, so it returns at the non-2xx
// arm; the decode at the bottom of fetchToken runs only on a 2xx — the response carrying
// access_token and the rotated refresh_token — and it was the last arm still wrapping its
// cause with %w.
//
// The concrete vector is a NUMBER literal: json.UnmarshalTypeError reproduces an
// out-of-range one verbatim and unbounded, so a hostile or broken upstream could push
// arbitrary-length digits into an error that persists in a campaign's Steps. A full
// credential is not expressible that way (any non-numeric byte fails as a syntax error
// first), which is why this is defence in depth — but the error must still describe shape
// rather than echo the body, as every other arm of fetchToken does.
func TestTokenDecodeErrorNeverEchoesTheResponseBody(t *testing.T) {
	const marker = "86400123456789012345678901234567890"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // 2xx: reach the decode, not the non-2xx arm
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":` + marker + `}`))
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))

	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("an undecodable token response must not read as a successful exchange")
	}

	// Every layer, not just the outermost: a credential-bearing wrapper is exactly what an
	// Error() check on the top layer alone would miss.
	for layer := err; layer != nil; layer = errors.Unwrap(layer) {
		text := layer.Error()
		if strings.Contains(text, marker) {
			t.Errorf("error layer %T echoes the response body verbatim: %v", layer, layer)
		}
		for _, s := range []string{"client-secret", "stored-refresh-token", "client_secret"} {
			if strings.Contains(text, s) {
				t.Errorf("error layer %T renders credential material %q: %v", layer, s, layer)
			}
		}
	}
}

// TestRefreshCapable401FailsClosedWithoutReplay pins the contract for the case the other
// 401 tests do not reach: a connection whose CanRefresh() is TRUE receiving a 401 from the
// API. Those tests build bearer-only credentials, so this decision had no regression seam.
//
// The contract is fail-closed with NO refresh-and-replay inside the failing operation, and
// that is deliberate rather than an oversight. doRequest already refuses to re-send a plain
// create POST on a 429 because those endpoints carry no idempotency key and the rejected
// attempt may have committed upstream; a 401 establishes no more about whether the write
// landed. The self-heal is the NEXT operation, which dispatch starts with a fresh Client and
// an empty cache.
//
// Asserting the exchange COUNT is the load-bearing half: without it the test would pass
// equally well against an implementation that silently retried.
func TestRefreshCapable401FailsClosedWithoutReplay(t *testing.T) {
	var exchanges, apiCalls atomic.Int32

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-token","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	// The API rejects EVERY attempt, so a replay would show up as a second call.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"serviceErrorCode":65602,"message":"The token used in the request has expired","status":401}`))
	}))
	defer apiSrv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{},
		withTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL))

	_, err := c.doRequest(context.Background(), http.MethodGet, "/adAccounts", nil, nil, nil)
	if !errors.Is(err, ErrCredentialsExpired) {
		t.Fatalf("error = %v, want ErrCredentialsExpired — a refresh-capable connection whose "+
			"freshly minted token is also rejected must still fail closed", err)
	}
	if got := apiCalls.Load(); got != 1 {
		t.Errorf("api calls = %d, want 1: the 401 must not be replayed inside the failing "+
			"operation — create POSTs carry no idempotency key", got)
	}
	// One exchange for the expired injected token; none afterwards for the 401 itself.
	if got := exchanges.Load(); got != 1 {
		t.Errorf("token exchanges = %d, want 1: the 401 arm must not trigger a second exchange", got)
	}
	// The rejected token is dropped, so the next operation re-exchanges rather than
	// replaying it. This is what "the next caller re-exchanges" actually buys.
	c.tokenMu.Lock()
	cached := c.accessToken
	c.tokenMu.Unlock()
	if cached != "" {
		t.Errorf("cached access token = %q, want empty: a rejected token must be invalidated", cached)
	}
}

// Redaction must not cost classification: a cancelled context still has to be detectable
// through the chain, which is why redactBodyReadError returns the canonical sentinels.
func TestTokenBodyReadCancellationRemainsDetectable(t *testing.T) {
	c := NewClient(refreshableCreds(), RuntimeConfig{},
		withTokenURL("https://example.invalid/oauth/v2/accessToken"),
		WithHTTPClient(&http.Client{Transport: cancelBodyRoundTripper{}}))

	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled preserved through redaction so callers can still classify it", err)
	}
}

type cancelBodyRoundTripper struct{}

func (cancelBodyRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(&ctxCancelReader{}),
	}, nil
}

type ctxCancelReader struct{}

func (*ctxCancelReader) Read([]byte) (int, error) { return 0, context.Canceled }

// TestPaddedClientCredentialsAreTrimmedOnTheWire pins the SEND half of the
// whitespace-padding class. CanRefresh() gates on the TRIMMED value, so a padded
// " client-id " satisfies every validator in the package; before this fix the same
// value was written RAW into the exchange form, and LinkedIn answered invalid_client
// on every refresh forever — an unrecoverable state a validator claimed to prevent.
//
// Asserting CanRefresh() would prove nothing here: it returns true both before and
// after the fix. The assertion therefore reads what actually reached the token
// endpoint's form body.
func TestPaddedClientCredentialsAreTrimmedOnTheWire(t *testing.T) {
	var mu sync.Mutex
	var gotClientID, gotSecret, gotRefresh string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		mu.Lock()
		gotClientID = form.Get("client_id")
		gotSecret = form.Get("client_secret")
		gotRefresh = form.Get("refresh_token")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(linkedInSampleTokenResponse))
	}))
	t.Cleanup(srv.Close)

	creds := refreshableCreds()
	creds.ClientID = "  client-id  "
	creds.ClientSecret = "\tclient-secret\n"
	creds.RefreshToken = " stored-refresh-token "

	// The padded trio still passes the validator, which is precisely the problem.
	if !creds.CanRefresh() {
		t.Fatal("padded credentials should still satisfy CanRefresh (it gates on the trimmed value)")
	}

	c := NewClient(creds, RuntimeConfig{}, withTokenURL(srv.URL))
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("accessTokenValue: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotClientID != "client-id" {
		t.Errorf("client_id on the wire = %q, want %q — a padded value is sent verbatim and LinkedIn rejects it as invalid_client", gotClientID, "client-id")
	}
	if gotSecret != "client-secret" {
		t.Errorf("client_secret on the wire = %q, want %q", gotSecret, "client-secret")
	}
	if gotRefresh != "stored-refresh-token" {
		t.Errorf("refresh_token on the wire = %q, want %q", gotRefresh, "stored-refresh-token")
	}
}

// TestInvalidClientIsNotReportedAsAnExpiredCredential pins the OAuth error-code split.
// A token endpoint answers 400/401 for BOTH a dead refresh token and a wrong client
// credential. Classifying on status alone told an operator whose client_id held a typo
// to "re-authorize the connection" — which can never help, because the refresh token
// was never the problem.
func TestInvalidClientIsNotReportedAsAnExpiredCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"client authentication failed for app 99 secret sk-LEAKME"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error for invalid_client")
	}

	// The operator must NOT be sent to re-authorize: that is the wrong remedy.
	if errors.Is(err, ErrCredentialsExpired) {
		t.Errorf("invalid_client classified as ErrCredentialsExpired; it is an application-credential misconfiguration, and re-authorizing the member cannot fix it (err = %v)", err)
	}
	// Being "not expired" is NOT enough, and asserting only that is what let this arm
	// regress into a bare fmt.Errorf that unwrapped to nothing. Every consumer classifies
	// STRUCTURALLY, so the error must carry its own sentinel — otherwise it is invisible to
	// all of them and falls through to the generic retryable 503, the opaque surface this
	// split exists to retire.
	if !errors.Is(err, ErrApplicationCredentialsInvalid) {
		t.Errorf("invalid_client does not unwrap to ErrApplicationCredentialsInvalid, so no caller can classify it and it lands on the generic retryable arm: %v", err)
	}
	// The upstream error_description is untrusted and must never be echoed.
	for _, leak := range []string{"sk-LEAKME", "client authentication failed for app"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("upstream token-endpoint text %q leaked into the error: %v", leak, err)
		}
	}
}

// TestInvalidGrantStillReportsAnExpiredCredential is the other half of the split: the
// genuinely-expired case must keep its existing, correct classification.
//
// `invalid_grant` is the ONLY RFC 6749 §5.2 code that describes a dead grant, so it is the
// only code here. An unreadable body is included deliberately — see the fallback case below.
// This test previously also listed `invalid_request`, which asserted the OLD binary split
// (everything-but-invalid_client is expired) and would have passed against the very defect
// the sibling test now pins.
func TestInvalidGrantStillReportsAnExpiredCredential(t *testing.T) {
	for _, body := range []string{
		`{"error":"invalid_grant","error_description":"refresh token expired"}`,
		`not json at all`,
		``,
		`{"error":"some_code_the_rfc_does_not_define"}`,
	} {
		t.Run(fmt.Sprintf("body=%.20q", body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
			_, err := c.accessTokenValue(context.Background())
			if !errors.Is(err, ErrCredentialsExpired) {
				t.Errorf("err = %v, want ErrCredentialsExpired — invalid_grant, and any body this "+
					"client cannot classify, keep the re-authorize remedy", err)
			}
		})
	}
}

// TestNonGrantOAuthCodesSplitByWhoCanRepairThem pins the THREE-way classification.
//
// This test previously asserted a two-way split, and it was WRONG for three of the five codes
// it covered. It required `invalid_request`, `unsupported_grant_type` and `invalid_scope` to
// unwrap to ErrApplicationCredentialsInvalid, which resolves to a connection-repair 409
// telling an operator that their stored client_id/client_secret are wrong. Those three
// describe the REQUEST or the PROTOCOL — LinkedIn never evaluated either credential — so the
// remedy was actionable and provably useless, the exact failure the invalid_client split was
// built to retire, one taxonomy level down. It passed against that defect because the fixture
// and the code shared the assumption that "not a dead grant" implies "an operator's fault".
//
// RFC 6749 §5.2 defines six codes and they split by WHO can repair them:
//
//   - invalid_grant                                  → the MEMBER re-authorizes.
//   - invalid_client, unauthorized_client            → an OPERATOR edits the connection.
//   - invalid_request, unsupported_grant_type,
//     invalid_scope                                  → WE fix the service.
//
// Each is asserted on BOTH 400 and 401, since LinkedIn uses either for the same fault, and
// each asserts the two sentinels it must NOT carry as well as the one it must — a test that
// only checks the positive passes against a classifier that returns everything.
func TestNonGrantOAuthCodesSplitByWhoCanRepairThem(t *testing.T) {
	cases := map[string]struct {
		want, notA, notB error
	}{
		"invalid_client":         {ErrApplicationCredentialsInvalid, ErrCredentialsExpired, ErrTokenRequestRejected},
		"unauthorized_client":    {ErrApplicationCredentialsInvalid, ErrCredentialsExpired, ErrTokenRequestRejected},
		"invalid_request":        {ErrTokenRequestRejected, ErrCredentialsExpired, ErrApplicationCredentialsInvalid},
		"unsupported_grant_type": {ErrTokenRequestRejected, ErrCredentialsExpired, ErrApplicationCredentialsInvalid},
		"invalid_scope":          {ErrTokenRequestRejected, ErrCredentialsExpired, ErrApplicationCredentialsInvalid},
	}
	for code, tc := range cases {
		for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
			t.Run(fmt.Sprintf("%s/%d", code, status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":"` + code + `","error_description":"app 99 secret sk-LEAKME"}`))
				}))
				t.Cleanup(srv.Close)

				c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
				_, err := c.accessTokenValue(context.Background())
				if !errors.Is(err, tc.want) {
					t.Errorf("%s must unwrap to %v — that is the only remedy that can repair it (err = %v)",
						code, tc.want, err)
				}
				for _, wrong := range []error{tc.notA, tc.notB} {
					if errors.Is(err, wrong) {
						t.Errorf("%s also classified as %v, which names a DIFFERENT owner: the caller "+
							"acts on the reason token, so carrying two hands out a remedy that cannot "+
							"work (err = %v)", code, wrong, err)
					}
				}
				// The classification travels; the upstream body never does.
				for _, leak := range []string{"sk-LEAKME", "app 99"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("upstream token-endpoint text %q leaked into the error: %v", leak, err)
					}
				}
			})
		}
	}
}

// TestTokenRequestRejectedNamesNoOperatorRemedy pins the MESSAGE, not only the sentinel.
//
// The sentinel decides the reason token; the message is what a human reads in a persisted
// campaign Step. An ErrTokenRequestRejected whose text told someone to correct their
// credentials would reproduce the defect this split fixed while every errors.Is assertion
// above stayed green.
func TestTokenRequestRejectedNamesNoOperatorRemedy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_scope"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("invalid_scope was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "defect in this service") {
		t.Errorf("the message must say the fault is OURS; an operator reading it otherwise goes and "+
			"audits a correct connection: %q", msg)
	}
	for _, wrongRemedy := range []string{"are wrong or unknown to LinkedIn", "expired, revoked or invalid"} {
		if strings.Contains(msg, wrongRemedy) {
			t.Errorf("the message carries a credential remedy (%q) for a fault in which neither "+
				"credential was evaluated: %q", wrongRemedy, msg)
		}
	}
}
