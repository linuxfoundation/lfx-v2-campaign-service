// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package googleads provides a Go client for the Google Ads API.
//
// It ports the upstream TypeScript Google Ads implementation (the google-ads-api
// gRPC usage in campaign-proxy.service.ts / campaign-metrics.service.ts) to a
// REST client that speaks the Google Ads REST transport directly:
//
//   - GAQL reads via POST customers/{id}/googleAds:search
//   - mutations via POST customers/{id}/{resource}:mutate
//
// REST (rather than the official gRPC SDK) is used deliberately so this client
// matches the meta/reddit/twitter/linkedin clients' structure and avoids a large
// generated gRPC dependency.
//
// Unlike a single-Bearer client, Google Ads auth requires an OAuth2 refresh-token
// exchange plus a developer token and (for manager access) a login-customer-id
// header on every call. Credentials and account configuration are injected via
// NewClient; the client never reads the process environment.
//
// This file is the client scaffold (GA-1): auth, the request layer, and GAQL
// search. Campaign creation (:mutate flows), metrics/keywords/audience reads, and
// keyword actions land in follow-up changes (GA-2..GA-4).
package googleads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Google Ads API constants
// ---------------------------------------------------------------------------

const (
	// googleAdsAPIVersion is the Google Ads API version segment in the URL path.
	// Google deprecates ~3 versions/year; bump deliberately and re-verify the
	// GAQL field set when doing so. (Known-good per the ported TS client: v23.)
	googleAdsAPIVersion = "v23"

	// googleAdsBaseURL is the Google Ads REST base (version appended per request).
	googleAdsBaseURL = "https://googleads.googleapis.com"

	// googleOAuthTokenURL is the OAuth 2.0 token endpoint used to exchange a
	// refresh token for a short-lived access token.
	googleOAuthTokenURL = "https://oauth2.googleapis.com/token"

	// googleAdsRequestTimeout bounds a single API call.
	googleAdsRequestTimeout = 30 * time.Second

	// tokenExpiryBuffer refreshes the access token this long before its stated
	// expiry so an in-flight request is not made with a just-expired token.
	tokenExpiryBuffer = 60 * time.Second

	// defaultTokenTTL is the fallback token lifetime used when the OAuth response
	// omits (or reports a non-positive) expires_in, so a valid-but-lifetimeless
	// token still works without caching an already-expired entry.
	defaultTokenTTL = 30 * time.Minute

	// maxResponseBytes bounds a response body read so an unexpectedly large
	// response cannot exhaust memory. Mirrors the meta/reddit clients.
	maxResponseBytes = 8 << 20 // 8 MiB

	// maxErrorBodyChars bounds how much of a non-2xx body is retained on apiError
	// for internal classification. The body is never surfaced by Error(); the cap
	// only keeps the retained value from bloating on a large error page.
	maxErrorBodyChars = 400

	// retryMax is the number of times an HTTP 429 (rate-limited) IDEMPOTENT
	// request is retried before giving up. Mirrors the meta/reddit/linkedin
	// clients.
	retryMax = 3
	// retryBaseDelay is the base for exponential backoff when a 429 carries no
	// usable Retry-After header (1s, 2s, 4s, …). Mirrors the sibling clients.
	retryBaseDelay = 1 * time.Second
	// maxRetryWait caps a single 429 backoff so an outsized server-declared reset
	// can't stall a request indefinitely.
	maxRetryWait = 60 * time.Second
)

// ---------------------------------------------------------------------------
// Credentials / configuration
// ---------------------------------------------------------------------------

// Credentials holds the OAuth2 + developer-token secrets required to call the
// Google Ads API. All are injected (never read from the environment).
//
// Which of these are stored encrypted vs as plain provider-config is a
// connection-layer concern; this client treats them all as injected inputs.
type Credentials struct {
	// ClientID / ClientSecret identify the OAuth2 application.
	ClientID     string
	ClientSecret string
	// DeveloperToken is the Google Ads API developer token, sent as the
	// `developer-token` header on every call.
	DeveloperToken string
	// RefreshToken is exchanged for a short-lived access token.
	RefreshToken string
}

// AccountConfig identifies the ad account the client operates on.
type AccountConfig struct {
	// CustomerID is the ad account's customer id, DIGITS ONLY (no dashes), e.g.
	// "1234567890". It is the {customerId} path segment.
	CustomerID string
	// LoginCustomerID is an OPTIONAL manager (MCC) account id (digits only) sent
	// as the `login-customer-id` header when the CustomerID is accessed through a
	// manager account. Empty means direct access (header omitted).
	LoginCustomerID string
	// Label is an optional human-readable account label surfaced in results.
	Label string
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client is a Google Ads API client for one ad account.
type Client struct {
	creds   Credentials
	account AccountConfig

	baseURL    string
	apiVersion string
	tokenURL   string

	httpClient *http.Client
	now        func() time.Time

	// retryBaseDelay is the base for exponential 429 backoff. Defaults to the
	// retryBaseDelay const; tests shrink it (via withRetryBaseDelay) to keep runs
	// fast.
	retryBaseDelay time.Duration

	// tokenMu guards the cached access token AND the inflight single-flight
	// pointer. It is held only for the brief cache read/write and to publish or
	// clear the inflight refresh — NEVER across the network call (see
	// accessTokenValue), so a slow token endpoint can't serialize every concurrent
	// call behind the refresher.
	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time

	// inflight coalesces concurrent token refreshes. The caller that finds the
	// cache empty/expired becomes the leader: it publishes a *tokenRefresh here
	// and runs the fetch on a detached context in a goroutine. Followers wait on
	// the shared tokenRefresh.done channel and read the shared result, so one
	// caller's cancellation can't tear down a refresh the others depend on, and a
	// failed refresh fails all current waiters at once rather than each re-leading
	// a serial refresh.
	inflight *tokenRefresh
}

// tokenRefresh holds the shared result of one in-flight token refresh. done is
// closed when the refresh completes; token/err carry the outcome.
type tokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

// Option customizes a Client.
type Option func(*Client)

// noFollow is the CheckRedirect policy for every client this package uses: it
// returns http.ErrUseLastResponse so the client does NOT follow redirects and
// hands the 3xx response back to the request layer, where a non-2xx status is
// surfaced as an error. Following a redirect could carry an already-committed
// mutating POST to a different target and muddy outcome classification. Mirrors
// the reddit/meta/linkedin clients' noFollow.
func noFollow(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// WithHTTPClient overrides the default *http.Client. Redirect following is
// force-disabled on whatever client ends up in use (see NewClient).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithBaseURL overrides the API base URL. Primarily for tests (httptest.Server).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithTokenURL overrides the OAuth2 token endpoint. Primarily for tests.
func WithTokenURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.tokenURL = u
		}
	}
}

// WithAPIVersion overrides the Google Ads API version segment in the URL path.
// Google rotates versions ~3x/year; this lets a deployment pin/bump the version
// without a code change, and lets tests assert the version reaches the path.
func WithAPIVersion(v string) Option {
	return func(c *Client) {
		if v != "" {
			c.apiVersion = v
		}
	}
}

// WithClock overrides the time source. For tests.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// withRetryBaseDelay overrides the exponential-backoff base for 429 retries.
// Unexported: only tests use it, to keep retry runs fast.
func withRetryBaseDelay(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.retryBaseDelay = d
		}
	}
}

// NewClient builds a Google Ads client from injected credentials and account
// config. Redirect following is force-disabled on whatever *http.Client is used,
// including one supplied via WithHTTPClient (applied to a shallow copy so the
// caller's client is not mutated). Mirrors the reddit/linkedin clients.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		account:        account,
		baseURL:        googleAdsBaseURL,
		apiVersion:     googleAdsAPIVersion,
		tokenURL:       googleOAuthTokenURL,
		httpClient:     &http.Client{Timeout: googleAdsRequestTimeout, CheckRedirect: noFollow},
		now:            time.Now,
		retryBaseDelay: retryBaseDelay,
	}
	for _, o := range opts {
		o(c)
	}
	if c.httpClient != nil {
		hc := *c.httpClient
		hc.CheckRedirect = noFollow
		c.httpClient = &hc
	}
	return c
}

// ---------------------------------------------------------------------------
// Error types (mirror the meta/reddit ambiguity contract)
// ---------------------------------------------------------------------------

// apiError is a non-2xx response from the Google Ads or OAuth endpoint. It
// carries status/method/path so an error names exactly which call failed. The
// upstream body is retained for internal classification but deliberately not
// surfaced in Error(), since it can reflect request material.
type apiError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("google-ads %s %s -> %d", e.Method, e.Path, e.StatusCode)
}

// transportError wraps a round-trip failure that happened AFTER the request was
// plausibly sent (mid-flight timeout, EOF, reset), OR a failure to read/decode a
// 2xx response: the server may or may not have processed the request, so the
// outcome is AMBIGUOUS. A pre-send failure (request build, pre-connect dial —
// see isPreSendDialError) is NOT wrapped as transportError. Mirrors the
// meta/reddit clients.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("google-ads %s %s: %v", e.Method, e.Path, e.Err)
}

func (e *transportError) Unwrap() error { return e.Err }

// isPreSendDialError reports whether a httpClient.Do error clearly happened
// BEFORE the request could be sent — a DNS resolution failure or a connect-time
// dial failure (connection refused / no route / network unreachable). Only these
// prove the request never reached Google, so a mutation definitely did not
// happen. Every other Do error (mid-flight timeout, EOF, reset) is ambiguous.
// Mirrors the reddit/meta clients.
func isPreSendDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		if errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EHOSTUNREACH) ||
			errors.Is(err, syscall.ENETUNREACH) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// OAuth2: refresh token -> access token
// ---------------------------------------------------------------------------

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
	TokenType   string `json:"token_type"`
}

// accessTokenValue returns a valid access token, refreshing via the OAuth2 token
// endpoint when the cached one is absent or within tokenExpiryBuffer of expiry.
//
// Concurrent callers are coalesced with a single-flight leader/follower pattern
// (mirrors the reddit client's refreshToken). The lock is NOT held across the
// network call: the fast path reads the cache under a brief lock, and every
// waiter (leader included) selects on its own ctx so a cancelled caller returns
// promptly with its context error instead of blocking on — or tearing down — the
// shared refresh. A failed refresh fails all current waiters at once rather than
// each re-leading a serial refresh (which would amplify rate-limit pressure).
func (c *Client) accessTokenValue(ctx context.Context) (string, error) {
	// A caller whose context is already done never triggers or joins a refresh.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c.tokenMu.Lock()
	// Fast path: reuse the cached token while it remains valid past the buffer.
	if c.accessToken != "" && c.now().Add(tokenExpiryBuffer).Before(c.tokenExpiry) {
		token := c.accessToken
		c.tokenMu.Unlock()
		return token, nil
	}

	inflight := c.inflight
	if inflight == nil {
		// Become the leader: publish the shared result and kick off the fetch on a
		// context detached from this caller's CANCELLATION (one caller's cancel must
		// not tear down a refresh other waiters depend on) but preserving its
		// request-scoped VALUES via context.WithoutCancel. No lock is held across
		// the network call.
		inflight = &tokenRefresh{done: make(chan struct{})}
		c.inflight = inflight
		refreshValuesCtx := context.WithoutCancel(ctx)
		go func() {
			fetchCtx, cancel := context.WithTimeout(refreshValuesCtx, googleAdsRequestTimeout)
			token, err := c.fetchToken(fetchCtx)
			cancel()

			c.tokenMu.Lock()
			inflight.token = token
			inflight.err = err
			c.inflight = nil
			close(inflight.done)
			c.tokenMu.Unlock()
		}()
	}
	c.tokenMu.Unlock()

	// Leader and followers alike wait on the shared result, selecting on their own
	// ctx so a cancelled caller returns promptly while the detached fetch still
	// completes and populates the cache for the others.
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-inflight.done:
		return inflight.token, inflight.err
	}
}

// fetchToken performs the actual OAuth2 refresh-token exchange and caches the
// result under tokenMu. It is only ever invoked from the leader's detached
// refresh goroutine, so at most one call is in flight at a time.
func (c *Client) fetchToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("client_id", c.creds.ClientID)
	form.Set("client_secret", c.creds.ClientSecret)
	form.Set("refresh_token", c.creds.RefreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A token-endpoint failure ran no mutation; surface it plainly (the caller
		// aborts before any create). Do NOT wrap as transportError.
		return "", fmt.Errorf("google-ads token refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if int64(buf.Len()) > maxResponseBytes {
		return "", fmt.Errorf("token response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do NOT echo the token-endpoint body: this request carried the client
		// id/secret and refresh token, and an OAuth/proxy diagnostic body is
		// untrusted and may reflect that credential material. This error can be
		// persisted into a campaign's Steps, so a leaked secret would be durable —
		// report status only.
		return "", fmt.Errorf("google-ads token refresh -> %d", resp.StatusCode)
	}

	var tok tokenResponse
	if err := json.Unmarshal(buf.Bytes(), &tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("google-ads token refresh returned an empty access_token")
	}

	// expires_in may be absent; default so a missing value doesn't pin a stale
	// token forever (nor cache an already-expired entry).
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}

	c.tokenMu.Lock()
	c.accessToken = tok.AccessToken
	c.tokenExpiry = c.now().Add(ttl)
	c.tokenMu.Unlock()
	return tok.AccessToken, nil
}

// ---------------------------------------------------------------------------
// Request layer
// ---------------------------------------------------------------------------

// customerIDRE matches a Google Ads customer id: digits only, no dashes. The
// connection's account_id is user-supplied and its Goa design only checks
// presence, so it must be validated here before being concatenated into a URL —
// a padded/dashed id yields an invalid request, and slash/dot input could alter
// the resource path.
var customerIDRE = regexp.MustCompile(`^[0-9]+$`)

// validateAccountIDs rejects a CustomerID (and, when set, LoginCustomerID) that
// isn't a digits-only id, before any request is built.
func (c *Client) validateAccountIDs() error {
	if !customerIDRE.MatchString(c.account.CustomerID) {
		return fmt.Errorf("invalid Google Ads customer id %q: must be digits only (no dashes)", c.account.CustomerID)
	}
	if c.account.LoginCustomerID != "" && !customerIDRE.MatchString(c.account.LoginCustomerID) {
		return fmt.Errorf("invalid Google Ads login-customer-id %q: must be digits only (no dashes)", c.account.LoginCustomerID)
	}
	return nil
}

// customerPath builds a Google Ads REST resource path scoped to this account's
// customer id, e.g. customerPath("googleAds:search") ->
// "customers/1234567890/googleAds:search". Centralizes the customer-id segment so
// the search and (GA-2+) :mutate paths don't re-concatenate it. Callers must have
// validated the customer id (see validateAccountIDs / doRequest).
func (c *Client) customerPath(action string) string {
	return "customers/" + c.account.CustomerID + "/" + action
}

// doRequest performs one Google Ads REST call against
// {baseURL}/{version}/{path}, attaching the bearer access token, developer
// token, and (when set) login-customer-id headers. body is JSON-encoded when
// non-nil. It returns the raw 2xx body bytes; non-2xx and transport failures are
// classified per the ambiguity contract.
//
// idempotent gates 429 retry behavior. Google Ads throttles under normal use, so
// a rate-limited IDEMPOTENT call (a GAQL :search read) is retried up to retryMax
// times with a bounded backoff honoring Retry-After. A NON-idempotent call (a
// :mutate that creates a paid resource) is NOT retried: the create endpoints have
// no idempotency key, so a 429 whose first attempt may already have committed
// upstream would double-create on retry. For those the 429 is returned as an
// apiError immediately (and createOutcomeAmbiguous, GA-2+, treats a mutating 429
// as "may exist"). Note: GAQL :search is POST-but-read-only, so the caller passes
// idempotent explicitly rather than inferring it from the HTTP method.
func (c *Client) doRequest(ctx context.Context, method, path string, body any, idempotent bool) ([]byte, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}

	var encoded []byte
	if body != nil {
		b, mErr := json.Marshal(body)
		if mErr != nil {
			return nil, fmt.Errorf("marshal request body: %w", mErr)
		}
		encoded = b
	}

	u := c.baseURL + "/" + c.apiVersion + "/" + strings.TrimPrefix(path, "/")

	for attempt := 0; attempt <= retryMax; attempt++ {
		var reqBody io.Reader
		if encoded != nil {
			reqBody = bytes.NewReader(encoded)
		}

		// Fetch the token INSIDE the loop: after a 429 backoff (up to maxRetryWait
		// per attempt) the token cached before the loop could have expired, so a
		// resumed retry would 401. accessTokenValue returns the cached token on the
		// fast path, so this is cheap when no refresh is due.
		token, err := c.accessTokenValue(ctx)
		if err != nil {
			return nil, err
		}

		// Bound EACH attempt with its own deadline (the caller ctx is the parent so
		// a real cancel/deadline still propagates). cancel() runs on every exit path.
		attemptCtx, cancel := context.WithTimeout(ctx, googleAdsRequestTimeout)

		req, err := http.NewRequestWithContext(attemptCtx, method, u, reqBody)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("developer-token", c.creds.DeveloperToken)
		if c.account.LoginCustomerID != "" {
			req.Header.Set("login-customer-id", c.account.LoginCustomerID)
		}
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			cancel()
			if isPreSendDialError(err) {
				return nil, fmt.Errorf("google-ads %s %s: %w", method, path, err)
			}
			return nil, &transportError{Method: method, Path: path, Err: err}
		}

		// Retry a 429 only for idempotent calls with attempts remaining.
		if resp.StatusCode == http.StatusTooManyRequests && attempt < retryMax && idempotent {
			wait := c.parseRetryAfter(resp)
			rawRetryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
			// Drain (bounded) before closing so net/http can reuse the connection.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
			_ = resp.Body.Close()
			cancel()
			// If the server DECLARED a reset longer than maxRetryWait, ABORT rather
			// than clamp-and-retry: a capped sleep can't clear the window, so retrying
			// would just 429 again and stall the caller (mirrors the meta/reddit/
			// twitter clients). Report the RAW header as authoritative.
			if wait >= overCapRetryAfter {
				return nil, &apiError{StatusCode: http.StatusTooManyRequests, Method: method, Path: path, Body: fmt.Sprintf("rate-limit reset (Retry-After: %q) exceeds max wait %s; aborting", rawRetryAfter, maxRetryWait)}
			}
			if wait <= 0 {
				wait = c.retryBaseDelay * time.Duration(1<<uint(attempt))
				if wait > maxRetryWait {
					wait = maxRetryWait
				}
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
			_ = resp.Body.Close()
			cancel()
			// A read failure on a 2xx is ambiguous (the mutation may have committed but
			// we can't read the result); a non-2xx read failure carries the status.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("read response body: %w", err)}
			}
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: fmt.Sprintf("read response body: %v", err)}
		}
		_ = resp.Body.Close()
		cancel()

		if int64(buf.Len()) > maxResponseBytes {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
			}
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: fmt.Sprintf("response exceeds %d bytes", maxResponseBytes)}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			text := buf.String()
			if len(text) > maxErrorBodyChars {
				text = text[:maxErrorBodyChars]
			}
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: text}
		}

		return buf.Bytes(), nil
	}

	// Exhausted retryMax retries, all 429 (idempotent path). Surface as a 429
	// apiError so the caller sees the rate-limit cause.
	return nil, &apiError{StatusCode: http.StatusTooManyRequests, Method: method, Path: path, Body: "rate limited: exhausted retries"}
}

// overCapRetryAfter is a sentinel (> maxRetryWait) that parseRetryAfter returns
// when the server-declared reset exceeds maxRetryWait. doRequest checks for it and
// ABORTS the 429 rather than clamping-and-retrying: sleeping only maxRetryWait
// cannot clear a longer window, so a retry would just 429 again and burn attempts
// while holding the caller. Mirrors the meta/reddit/twitter clients (which abort
// on an over-cap reset). The RAW Retry-After header — not this sentinel — is
// reported in the abort error, so a huge reset isn't misprinted as "1m1s".
const overCapRetryAfter = maxRetryWait + time.Second

// parseRetryAfter returns the delay a 429's Retry-After header requests. It
// accepts both the numeric (delta-seconds) and HTTP-date forms. A missing,
// malformed, or non-positive value returns 0 (caller falls back to exponential
// backoff). A value EXCEEDING maxRetryWait returns the overCapRetryAfter sentinel
// so the caller can abort. Mirrors the sibling clients.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// A numeric value that overflows int64 is still numeric (not an HTTP-date):
		// positive overflow is an over-cap reset → sentinel; negative → no wait.
		if errors.Is(err, strconv.ErrRange) {
			if n == math.MaxInt64 {
				return overCapRetryAfter
			}
			return 0
		}
	} else if n > 0 {
		if n > int64(maxRetryWait/time.Second) {
			return overCapRetryAfter
		}
		return time.Duration(n) * time.Second
	} else {
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			if d > maxRetryWait {
				return overCapRetryAfter
			}
			return d
		}
	}
	return 0
}

// sleepCtx waits for d, returning early with the context error if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// GAQL search
// ---------------------------------------------------------------------------

// searchRequest is the POST body for customers/{id}/googleAds:search.
type searchRequest struct {
	Query string `json:"query"`
	// PageToken carries cursor pagination; empty on the first page.
	PageToken string `json:"pageToken,omitempty"`
}

// searchResponse is the (subset of the) googleAds:search response we consume.
// Rows are opaque JSON objects (GAQL SELECT shapes vary per query); callers
// decode the fields they asked for.
type searchResponse struct {
	Results       []json.RawMessage `json:"results"`
	NextPageToken string            `json:"nextPageToken"`
}

// maxSearchPages bounds cursor pagination so a server returning an endless
// nextPageToken can't loop forever. Generous — real report queries page out well
// before this.
const maxSearchPages = 1000

// maxSearchRows and maxSearchBytes cap the accumulated result set across all
// pages. The per-response maxResponseBytes cap only bounds ONE page, so without an
// aggregate cap a query that pages many times could retain enough to OOM the
// service. BOTH caps are needed: a row cap alone doesn't bound memory (one page's
// worth of rows can be near maxResponseBytes each, so 200k rows could still be
// gigabytes), and a byte cap alone allows pathological tiny-row counts. A query
// exceeding either aborts rather than silently truncating; callers needing more
// should narrow the query or (GA-3+) consume via a page callback.
//
// Package vars (not consts) only so tests can shrink them to exercise the abort
// branches without generating gigabytes of fixture data; production never changes
// them.
var (
	maxSearchRows  = 200_000
	maxSearchBytes = 64 << 20 // 64 MiB total across all retained pages
)

// gaqlSearch runs a GAQL query against this account and returns every result row
// (following cursor pagination to exhaustion). Each row is a raw JSON object the
// caller decodes according to its SELECT clause.
//
// NOTE: in Google Ads API v23, campaign.start_date / campaign.end_date were
// REPLACED by campaign.start_date_time / campaign.end_date_time — the old fields
// are rejected as unrecognized. Select the *_date_time fields for campaign
// schedule; a reporting date-range window (segments.date, or the query's
// DURING clause) is a separate concern and not a substitute for them.
func (c *Client) gaqlSearch(ctx context.Context, query string) ([]json.RawMessage, error) {
	path := c.customerPath("googleAds:search")
	var out []json.RawMessage
	var totalBytes int
	pageToken := ""
	seen := map[string]struct{}{}

	for page := 0; page < maxSearchPages; page++ {
		// GAQL search is read-only (idempotent), so a 429 is safe to retry.
		raw, err := c.doRequest(ctx, http.MethodPost, path, searchRequest{Query: query, PageToken: pageToken}, true /* idempotent */)
		if err != nil {
			return nil, fmt.Errorf("gaql search: %w", err)
		}
		var sr searchResponse
		if err := json.Unmarshal(raw, &sr); err != nil {
			// A 2xx search response we can't decode is wrapped as transportError for
			// contract-uniformity with the mutating clients. GAQL search is READ-only,
			// so there's no "did the mutation commit?" ambiguity here — this is simply
			// a malformed-but-received response the caller must not treat as an empty
			// result set. The uniform type keeps the mutate flows (GA-2+) and reads on
			// one classification path.
			return nil, &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("decode search response: %w", err)}
		}
		out = append(out, sr.Results...)
		// Count the WHOLE page payload (raw response bytes), not just result rows,
		// toward the byte cap. This covers everything the loop accumulates across
		// pages — including the nextPageToken strings retained in `seen` — so a
		// malformed server that returns many large tokens (or otherwise-bloated
		// pages) with few/no rows still trips the OOM guard.
		totalBytes += len(raw)
		if len(out) > maxSearchRows {
			return nil, fmt.Errorf("gaql search %q: result set exceeds %d rows — narrow the query", path, maxSearchRows)
		}
		if totalBytes > maxSearchBytes {
			return nil, fmt.Errorf("gaql search %q: accumulated response exceeds %d bytes — narrow the query", path, maxSearchBytes)
		}

		if sr.NextPageToken == "" {
			return out, nil
		}
		// Guard against a server that repeats the same non-empty cursor.
		if _, dup := seen[sr.NextPageToken]; dup {
			return nil, fmt.Errorf("gaql search %q: server repeated page token — aborting", path)
		}
		seen[sr.NextPageToken] = struct{}{}
		pageToken = sr.NextPageToken
	}
	return nil, fmt.Errorf("gaql search %q: exceeded %d pages with a page token still present", path, maxSearchPages)
}
