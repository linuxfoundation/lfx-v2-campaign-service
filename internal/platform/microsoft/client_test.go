// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testCreds() Credentials {
	return Credentials{
		ClientID:       "cid",
		ClientSecret:   "csecret",
		DeveloperToken: "devtok",
		RefreshToken:   "rtok",
	}
}

func testAccount() AccountConfig {
	return AccountConfig{AccountID: "1234567", CustomerID: "9876543", Label: "Test"}
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// tokenHandler answers the OAuth2 token endpoint with a valid access token.
func tokenHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
}

// ---- token layer ----------------------------------------------------------

func TestAccessToken_RefreshAndCache(t *testing.T) {
	var tokenCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("token request Content-Type = %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err == nil {
			if r.Form.Get("grant_type") != "refresh_token" {
				t.Errorf("grant_type = %q, want refresh_token", r.Form.Get("grant_type"))
			}
			if !strings.Contains(r.Form.Get("scope"), "msads.manage") {
				t.Errorf("scope missing msads.manage: %q", r.Form.Get("scope"))
			}
		}
		tokenHandler(w, r)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))

	tok, err := c.accessTokenValue(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if tok != "at-123" {
		t.Errorf("token = %q, want at-123", tok)
	}
	// Second call within expiry must reuse the cached token (no new HTTP call).
	if _, err := c.accessTokenValue(context.Background()); err != nil {
		t.Fatalf("second token: %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Errorf("token endpoint called %d times, want 1 (cached)", got)
	}
}

// TestAccessToken_SingleFlightCoalescesConcurrent verifies the leader/follower
// single-flight: many concurrent callers trigger exactly ONE token HTTP call, a
// follower cancelled mid-refresh returns promptly with its context error, and the
// remaining waiters still complete successfully from the shared refresh. Run under
// -race to catch token-cache data races.
func TestAccessToken_SingleFlightCoalescesConcurrent(t *testing.T) {
	var tokenCalls int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		<-release // block so all callers pile up behind one in-flight refresh
		tokenHandler(w, nil)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))

	const followers = 8
	results := make(chan error, followers)
	// One follower is cancellable; cancel it while the refresh is still blocked.
	cancelCtx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := c.accessTokenValue(cancelCtx)
		results <- err
	}()
	for i := 0; i < followers-1; i++ {
		go func() {
			_, err := c.accessTokenValue(context.Background())
			results <- err
		}()
	}

	// Give the goroutines a moment to all register as waiters, then cancel one.
	// The cancelled follower must return promptly even though the refresh is blocked.
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		// The first result to arrive should be the cancelled follower.
		t.Errorf("cancelled follower should return context.Canceled promptly, got: %v", err)
	}

	// Now release the refresh; the remaining followers succeed.
	close(release)
	for i := 0; i < followers-1; i++ {
		if err := <-results; err != nil {
			t.Errorf("a follower failed: %v", err)
		}
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Errorf("token endpoint called %d times, want exactly 1 (single-flight)", got)
	}
}

func TestAccessToken_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))
	if _, err := c.accessTokenValue(context.Background()); err == nil {
		t.Fatal("expected an error on a 401 token response, got nil")
	}
}

// TestAccessToken_ErrorDoesNotLeakSecrets verifies the token-refresh error path
// never echoes the OAuth response body — which is untrusted and can reflect the
// client_secret / refresh_token this request carried.
func TestAccessToken_ErrorDoesNotLeakSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","secret":"LEAK-csecret-rtok"}`)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(srv.URL), WithClock(fixedClock()))
	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "LEAK") || strings.Contains(err.Error(), "csecret") {
		t.Errorf("token error must not echo the response body/secrets, got: %v", err)
	}
}

// TestAccessToken_TransportErrorDoesNotLeakSecrets: a custom RoundTripper (allowed via
// WithHTTPClient) can return a Do error whose text echoes the request BODY — which carries
// client_secret + refresh_token. Wrapping that with %w would leak them into the returned/
// persisted error. Assert the surfaced error text carries NO secret, while Unwrap still
// exposes the cause (errors.Is on a sentinel).
func TestAccessToken_TransportErrorDoesNotLeakSecrets(t *testing.T) {
	leak := errors.New("dial tcp: post body client_secret=LEAK-csecret refresh_token=LEAK-rtok")
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL("http://token.invalid/oauth"),
		WithClock(fixedClock()),
		WithHTTPClient(&http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, leak
		})}),
	)
	_, err := c.accessTokenValue(context.Background())
	if err == nil {
		t.Fatal("expected a token transport error")
	}
	if strings.Contains(err.Error(), "LEAK") || strings.Contains(err.Error(), "csecret") || strings.Contains(err.Error(), "rtok") {
		t.Errorf("token transport error must not echo the RoundTripper's error text/secrets, got: %v", err)
	}
	// Unwrap must still preserve the cause so callers can errors.Is it.
	if !errors.Is(err, leak) {
		t.Errorf("token transport error must Unwrap to the original cause (for errors.Is), got: %v", err)
	}
}

func TestAccessToken_CancelledContextReturnsPromptly(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.accessTokenValue(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context must return context.Canceled, got: %v", err)
	}
}

// ---- request layer --------------------------------------------------------

// newAPIClient wires a client to a token server + an API server whose handler is
// supplied per-test.
func newAPIClient(t *testing.T, apiHandler http.HandlerFunc) *Client {
	t.Helper()
	tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tok.Close)
	api := httptest.NewServer(apiHandler)
	t.Cleanup(api.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tok.URL), WithBaseURL(api.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

func TestDoRequest_SendsIdentityHeadersAndPath(t *testing.T) {
	var gotAuth, gotDev, gotAcct, gotCust, gotPath string
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDev = r.Header.Get("DeveloperToken")
		gotAcct = r.Header.Get("CustomerAccountId")
		gotCust = r.Header.Get("CustomerId")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	if _, err := c.doRequest(context.Background(), http.MethodPost, "Campaigns", map[string]any{"x": 1}, false); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if gotAuth != "Bearer at-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotDev != "devtok" {
		t.Errorf("DeveloperToken = %q", gotDev)
	}
	if gotAcct != "1234567" {
		t.Errorf("CustomerAccountId = %q, want the account id", gotAcct)
	}
	if gotCust != "9876543" {
		t.Errorf("CustomerId = %q, want the customer id", gotCust)
	}
	if !strings.HasSuffix(gotPath, "/CampaignManagement/v13/Campaigns") {
		t.Errorf("path = %q, want it to carry the version + Campaigns", gotPath)
	}
}

func TestDoRequest_OmitsCustomerIdHeaderWhenUnset(t *testing.T) {
	var present bool
	tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tok.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Customerid"] // canonical MIME key for CustomerId
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(api.Close)
	c := NewClient(testCreds(), AccountConfig{AccountID: "1234567"},
		WithTokenURL(tok.URL), WithBaseURL(api.URL), WithClock(fixedClock()))
	if _, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if present {
		t.Error("CustomerId header must be omitted when the customer id is unset")
	}
}

func TestDoRequest_RetriesIdempotent429(t *testing.T) {
	var calls int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ErrorCode":"RateExceeded"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	// idempotent=true → the 429 is retried and the 2nd attempt succeeds.
	if _, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true); err != nil {
		t.Fatalf("idempotent request should retry past a 429, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 attempts (1 x 429 + 1 success), got %d", got)
	}
}

// TestDoRequest_AbortsOnOverCapRetryAfter verifies that a 429 whose server-declared
// Retry-After exceeds maxRetryWait ABORTS after a single attempt (returning the 429
// apiError) rather than clamping the wait and retrying into a guaranteed second 429.
func TestDoRequest_AbortsOnOverCapRetryAfter(t *testing.T) {
	var calls int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		// 600s ≫ maxRetryWait (60s): a clamped retry could never clear this window.
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ErrorCode":"RateExceeded"}`)
	})
	// idempotent=true, but the over-cap Retry-After must still abort after one attempt.
	_, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true)
	if err == nil {
		t.Fatal("expected a 429 error, got nil")
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected a 429 apiError, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("an over-cap Retry-After must abort after 1 attempt, got %d", got)
	}
}

func TestDoRequest_DoesNotRetryNonIdempotent429(t *testing.T) {
	var calls int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ErrorCode":"RateExceeded"}`)
	})
	// idempotent=false → a 429 on a create must NOT retry (it may have committed).
	_, err := c.doRequest(context.Background(), http.MethodPost, "Campaigns", map[string]any{}, false)
	if err == nil {
		t.Fatal("expected the 429 to be returned, not retried")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("a non-idempotent 429 must not retry; got %d attempts", got)
	}
	// ...and it must be classified as ambiguous (the throttled create may have committed).
	if !createOutcomeAmbiguous(err) {
		t.Errorf("a mutating 429 must be ambiguous, got: %v", err)
	}
}

func TestDoRequest_ErrorDoesNotLeakBody(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"Message":"contains SECRET-TOKEN","ErrorCode":"CampaignServiceInvalidBudget"}`)
	})
	_, err := c.doRequest(context.Background(), http.MethodPost, "Campaigns", map[string]any{}, false)
	if err == nil {
		t.Fatal("expected an error on a 400")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("apiError.Error() must not surface the response body, got: %v", err)
	}
	// The error code IS retained internally for classification.
	var ae *apiError
	if !errors.As(err, &ae) || !ae.hasErrorCode("CampaignServiceInvalidBudget") {
		t.Errorf("the error code must be retained for classification, got: %v", err)
	}
}

// ---- error classification -------------------------------------------------

func TestCreateOutcomeAmbiguous(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport", &transportError{Method: http.MethodPost, Path: "Campaigns", err: io.ErrUnexpectedEOF}, true},
		{"500-POST", &apiError{StatusCode: http.StatusInternalServerError, Method: http.MethodPost}, true},
		{"429-POST", &apiError{StatusCode: http.StatusTooManyRequests, Method: http.MethodPost}, true},
		{"302-POST", &apiError{StatusCode: http.StatusFound, Method: http.MethodPost}, true},
		{"302-GET", &apiError{StatusCode: http.StatusFound, Method: http.MethodGet}, false},
		{"400-POST", &apiError{StatusCode: http.StatusBadRequest, Method: http.MethodPost}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := createOutcomeAmbiguous(tc.err); got != tc.want {
			t.Errorf("%s: createOutcomeAmbiguous = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsDefiniteClientError(t *testing.T) {
	if !isDefiniteClientError(&apiError{StatusCode: http.StatusBadRequest}) {
		t.Error("a 400 must be a definite client error")
	}
	if isDefiniteClientError(&apiError{StatusCode: http.StatusTooManyRequests}) {
		t.Error("a 429 must NOT be a definite client error (it's ambiguous)")
	}
	if isDefiniteClientError(&apiError{StatusCode: http.StatusInternalServerError}) {
		t.Error("a 500 must NOT be a definite client error")
	}
}

func TestParseErrorCodes(t *testing.T) {
	// Top-level string ErrorCode.
	if got := parseErrorCodes([]byte(`{"ErrorCode":"CampaignServiceInvalidBudget"}`)); len(got) != 1 || got[0] != "CampaignServiceInvalidBudget" {
		t.Errorf("top-level ErrorCode = %v", got)
	}
	// Numeric Code normalized to a string.
	if got := parseErrorCodes([]byte(`{"Code":509}`)); len(got) != 1 || got[0] != "509" {
		t.Errorf("numeric Code = %v", got)
	}
	// Nested OperationErrors / PartialErrors.
	body := `{"OperationErrors":[{"ErrorCode":"CampaignServiceEditorialError"}],"PartialErrors":[{"Code":1100}]}`
	got := parseErrorCodes([]byte(body))
	if len(got) != 2 {
		t.Fatalf("nested codes = %v, want 2", got)
	}
	// BatchErrors (the v13 ApiFaultDetail per-list-item fault array) must be visited —
	// a code present only there (e.g. a duplicate-campaign-name fault) would otherwise
	// be silently dropped.
	if bg := parseErrorCodes([]byte(`{"BatchErrors":[{"ErrorCode":"CampaignServiceCannotCreateDuplicateCampaign"}]}`)); len(bg) != 1 || bg[0] != "CampaignServiceCannotCreateDuplicateCampaign" {
		t.Errorf("BatchErrors code = %v, want the duplicate-campaign code", bg)
	}
	// Malformed / empty bodies are nil.
	if parseErrorCodes(nil) != nil || parseErrorCodes([]byte(`{bad`)) != nil {
		t.Error("malformed/empty bodies must parse to nil")
	}
}

func TestValidateAccountIDs(t *testing.T) {
	if err := (&Client{account: AccountConfig{AccountID: "1234567"}}).validateAccountIDs(); err != nil {
		t.Errorf("a numeric account id must be valid: %v", err)
	}
	for _, bad := range []string{"", "12-34", "abc", "12 34", "12/34"} {
		if err := (&Client{account: AccountConfig{AccountID: bad}}).validateAccountIDs(); err == nil {
			t.Errorf("account id %q must be rejected", bad)
		}
	}
	// A non-numeric CustomerID (when set) is also rejected.
	if err := (&Client{account: AccountConfig{AccountID: "1", CustomerID: "bad/id"}}).validateAccountIDs(); err == nil {
		t.Error("a non-numeric customer id must be rejected")
	}
	// An oversized invalid id must not embed its full (unbounded, user-supplied) value in
	// the error — it is clipped so a huge id can't bloat a returned/logged/persisted error.
	huge := strings.Repeat("x", 5000)
	err := (&Client{account: AccountConfig{AccountID: huge}}).validateAccountIDs()
	if err == nil {
		t.Fatal("an oversized non-numeric account id must be rejected")
	}
	if len(err.Error()) > 200 {
		t.Errorf("error embeds the full oversized id (%d bytes); want it clipped", len(err.Error()))
	}
}

func TestIsPreSendDialError(t *testing.T) {
	if !isPreSendDialError(&net.DNSError{Err: "no such host"}) {
		t.Error("a DNS error must be pre-send")
	}
	if isPreSendDialError(io.ErrUnexpectedEOF) {
		t.Error("a mid-flight EOF must NOT be pre-send (it's ambiguous)")
	}
}

func TestParseNonNegativeInt(t *testing.T) {
	if n, err := parseNonNegativeInt("42"); err != nil || n != 42 {
		t.Errorf("parseNonNegativeInt(42) = %d, %v", n, err)
	}
	for _, bad := range []string{"", "-1", "1.5", "Wed, 21 Oct 2015 07:28:00 GMT", "10x"} {
		if _, err := parseNonNegativeInt(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	// Overflow must be REJECTED, not wrapped to a small positive value. MaxInt64 is
	// accepted; MaxInt64+1 and a value that wraps past zero (e.g. 18446744073709551617)
	// must error rather than silently becoming a tiny number.
	if n, err := parseNonNegativeInt("9223372036854775807"); err != nil || n != 9223372036854775807 {
		t.Errorf("MaxInt64 must parse exactly, got %d, %v", n, err)
	}
	for _, over := range []string{"9223372036854775808", "18446744073709551617", "99999999999999999999"} {
		if _, err := parseNonNegativeInt(over); err == nil {
			t.Errorf("%q must be rejected as overflow (not wrapped)", over)
		}
	}
}

// TestParseRetryAfter_HugeDeltaSecondsAborts verifies an enormous delta-seconds
// Retry-After that would overflow secs*time.Second is treated as over-cap (aborts),
// not wrapped to a non-positive/short wait.
func TestParseRetryAfter_HugeDeltaSecondsAborts(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	resp := &http.Response{Header: http.Header{}}
	// In-range-but-over-cap (parses fine, exceeds maxRetryWaitSeconds) → abort.
	resp.Header.Set("Retry-After", "9223372036854775807") // MaxInt64 seconds
	if got := c.parseRetryAfter(resp); got != overCapRetryAfter {
		t.Errorf("a huge delta-seconds Retry-After must map to overCapRetryAfter, got %v", got)
	}
	// OVERFLOW: an all-digit value too large for int64 fails parseNonNegativeInt, but is
	// still a delta-seconds value far beyond the cap → must abort, not fall through to 0
	// (which would trigger ordinary backoff-and-retry).
	for _, over := range []string{"9223372036854775808", "99999999999999999999999999"} {
		resp.Header.Set("Retry-After", over)
		if got := c.parseRetryAfter(resp); got != overCapRetryAfter {
			t.Errorf("an overflowing delta-seconds Retry-After %q must map to overCapRetryAfter, got %v", over, got)
		}
	}
	// A garbage (non-digit, non-date) value is genuinely unparseable → 0 (fall back to
	// exponential backoff).
	resp.Header.Set("Retry-After", "soon-ish")
	if got := c.parseRetryAfter(resp); got != 0 {
		t.Errorf("an unparseable Retry-After should be 0, got %v", got)
	}
	// A normal in-range value passes through unchanged.
	resp.Header.Set("Retry-After", "5")
	if got := c.parseRetryAfter(resp); got != 5*time.Second {
		t.Errorf("Retry-After 5 = %v, want 5s", got)
	}
}

// TestStatusAwareReadError verifies a read/oversize failure keeps its known status:
// a 2xx is an ambiguous transportError, a non-2xx keeps its apiError status.
func TestStatusAwareReadError(t *testing.T) {
	c := NewClient(testCreds(), testAccount())
	// 2xx read failure → transportError (ambiguous).
	if err := c.statusAwareReadError(200, http.MethodPost, "Campaigns", io.ErrUnexpectedEOF); !createOutcomeAmbiguous(err) {
		t.Errorf("a 2xx read failure must be ambiguous (transportError), got: %v", err)
	}
	// 400 read failure → apiError with status preserved, definite (not ambiguous).
	err := c.statusAwareReadError(400, http.MethodPost, "Campaigns", io.ErrUnexpectedEOF)
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != 400 {
		t.Errorf("a 400 read failure must keep its status as apiError, got: %v", err)
	}
	if createOutcomeAmbiguous(err) {
		t.Errorf("a definite 400 must NOT be ambiguous, got: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string must be returned as-is, got %q", got)
	}
	if got := truncate("hello", 3); got != "hel" {
		t.Errorf("truncate to 3 = %q, want hel", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Errorf("n<=0 must return empty, got %q", got)
	}
	// Multibyte: must cut on a rune boundary, never split a rune. "héllo" (é is 2 bytes)
	// truncated to 2 runes is "hé", which must remain valid UTF-8.
	got := truncate("héllo", 2)
	if got != "hé" {
		t.Errorf("multibyte truncate = %q, want hé", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncate must not split a rune; %q is not valid UTF-8", got)
	}
	// A body at exactly the maxErrorBodyChars cap is truncated to the cap in runes.
	long := strings.Repeat("x", maxErrorBodyChars+50)
	if n := utf8.RuneCountInString(truncate(long, maxErrorBodyChars)); n != maxErrorBodyChars {
		t.Errorf("over-limit truncate kept %d runes, want %d", n, maxErrorBodyChars)
	}
}

// errReadCloser is a response body whose Read always fails, to simulate a body-read
// error on a response whose status line was already received.
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errReadCloser) Close() error             { return nil }

// rtFunc adapts a function to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDoRequest_429BodyReadFailureStillRetries verifies that when a 429's body read
// fails, an idempotent call STILL follows the bounded retry path (the status line
// warrants it) rather than exiting immediately. The first attempt is a 429 with an
// unreadable body; the second returns a clean 200.
func TestDoRequest_429BodyReadFailureStillRetries(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tok.Close)

	var calls int32
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		// Only the API calls (not the token endpoint) are counted/branched.
		if strings.Contains(r.URL.Path, "/CampaignManagement/") {
			if atomic.AddInt32(&calls, 1) == 1 {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": []string{"0"}},
					Body:       errReadCloser{}, // read fails
					Request:    r,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    r,
			}, nil
		}
		// token endpoint
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"at","expires_in":3600}`)), Request: r}, nil
	})

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tok.URL), WithBaseURL("http://ms.invalid"),
		WithHTTPClient(&http.Client{Transport: transport}),
		WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))

	if _, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true); err != nil {
		t.Fatalf("idempotent 429-with-unreadable-body should retry to success, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 API attempts (429-read-fail then 200), got %d", got)
	}
}
