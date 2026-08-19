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
	var (
		gotMethod, gotContentType string
		gotForm                   url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(linkedInSampleTokenResponse))
	}))
	defer srv.Close()

	c := NewClient(refreshableCreds(), RuntimeConfig{}, withTokenURL(srv.URL))
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want x-www-form-urlencoded", gotContentType)
	}
	for k, want := range map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "stored-refresh-token",
		"client_id":     "client-id",
		"client_secret": "client-secret",
	} {
		if got := gotForm.Get(k); got != want {
			t.Errorf("form[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestAccessTokenValueAdoptsRotatedRefreshToken proves the refresh_token returned by
// the exchange is adopted for the NEXT exchange. LinkedIn returns the refresh token
// on every exchange; ignoring it would keep using a superseded value.
func TestAccessTokenValueAdoptsRotatedRefreshToken(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen = append(seen, r.PostForm.Get("refresh_token"))
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

	if len(seen) != 2 {
		t.Fatalf("exchanges = %d, want 2", len(seen))
	}
	if seen[0] != "stored-refresh-token" {
		t.Errorf("first exchange used %q, want the stored token", seen[0])
	}
	if seen[1] != "rotated-refresh-token" {
		t.Errorf("second exchange used %q, want the rotated token adopted from the response", seen[1])
	}
}

// TestAccessTokenValueRejectedRefreshIsActionable proves a rejected refresh token
// fails CLOSED with ErrCredentialsExpired naming the connection — not a 500, and
// never a fallback to the stale token.
func TestAccessTokenValueRejectedRefreshIsActionable(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"refresh token is invalid, expired or revoked"}`))
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
