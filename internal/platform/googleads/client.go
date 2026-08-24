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
	"unicode/utf8"
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
	// Body is a TRUNCATED (maxErrorBodyChars) snapshot of the error body, retained
	// only for logging/diagnostics and never surfaced by Error(). Do NOT parse it
	// for classification — it may be cut mid-JSON. Use ErrorCodes instead.
	Body string
	// ErrorCodes holds Google's machine-readable error-code enum constants, parsed
	// from the FULL (untruncated) error body in doRequest before Body is truncated.
	// This is what hasErrorCode matches on, so duplicate/field-error detection works
	// even for error payloads longer than maxErrorBodyChars.
	ErrorCodes []string
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

// invalidateAccessToken drops the cached token so the NEXT accessTokenValue mints a fresh one.
//
// It exists because a token's ADVERTISED expiry is not the only way it stops working. A
// revoked, rotated, or server-side-invalidated token keeps its expires_in, so the fast path in
// accessTokenValue goes on serving it long after the platform started rejecting it. Nothing
// else ever clears the cache, so without this the client is stuck for the remainder of that
// advertised lifetime.
//
// This became reachable when the dispatcher started CACHING clients across operations
// (internal/dispatch/googleads.go). A client rebuilt per operation began with an empty cache,
// so one rejection cost one failure; a cached client re-presents the same rejected token to
// every later call. On a dispatch path that spends money, that is a stuck campaign rather than
// a transient error.
//
// It clears the EXPIRY as well as the token, so the cache is empty by either half of the fast
// path's condition and a future edit to that test cannot silently resurrect the token.
//
// An in-flight single-flight refresh is NOT left alone, because its result can be the very
// token this 401 rejected. fetchToken stores the token and UNLOCKS; only under a LATER lock
// acquisition does the leader publish it on the flight and retract c.inflight. In that window
// the cache already holds the new token while the flight is still joinable, so a caller that
// missed the cache would be handed the rejected value even after the cache was cleared. A
// flight whose token MATCHES is therefore blanked and unpublished as well.
func (c *Client) invalidateAccessToken(presented string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	// Compare-and-clear. The cache may already hold a NEWER token than the one this 401
	// answered: with a shared client, request A can leave carrying tok_1, request B can
	// refresh and cache tok_2, and A's late 401 then arrives describing tok_1. An
	// unconditional clear would evict tok_2 — a token nothing rejected — and a burst of
	// late responses would drive serial re-exchanges, defeating the single-flight
	// coalescing this client already has.
	//
	// Clearing only on a match makes the operation idempotent and self-limiting: the
	// rejected token is dropped exactly once, and every later 401 naming it is a no-op
	// because the cache has already moved on.
	if presented == "" {
		return
	}

	// A live single-flight refresh that has ALREADY produced this token must be poisoned
	// too, not just the cache. fetchToken stores the token and UNLOCKS; only under a LATER
	// lock acquisition does the leader set inflight.token, clear c.inflight and close done.
	// In that window a fast-path caller can take the new token, be rejected, and clear the
	// cache here — while a caller that missed the cache joins the still-published flight
	// and is handed the very same rejected token. Clearing the cache alone would let the
	// rejection leak straight back out.
	//
	// This check is deliberately INDEPENDENT of the cache comparison below: once the cache
	// has been cleared, gating it behind a cache match would skip exactly the case it
	// exists to cover.
	//
	// It stays as SELECTIVE as the cache clear — matching on the token's identity, never
	// blanking a flight unconditionally — so a flight carrying a token no 401 named is
	// untouched and the ABA over-invalidation described above is not reintroduced.
	//
	// The token is blanked rather than an error being set: waiters treat an empty token as
	// a miss and re-lead a fresh exchange, which is the wanted recovery, and it keeps this
	// function total (it invents no error text for a response it never saw).
	if c.inflight != nil && c.inflight.token == presented {
		c.inflight.token = ""
		// UNPUBLISH the poisoned flight as well as blanking it. Blanking alone is not
		// enough: a waiter that re-leads would find this same flight still on c.inflight,
		// rejoin it, read the blank again and recurse without bound (a stack overflow, not
		// a retry). Detaching it means the next caller finds no flight and starts a
		// genuinely NEW exchange.
		//
		// Dropping the pointer is safe: done is closed exactly once by the leader that owns
		// this value, and it still holds its own local reference, so unpublishing here
		// cannot double-close or strand an existing waiter — waiters already blocked on
		// done are released as normal and then take the poisoned arm themselves.
		c.inflight = nil
	}

	if c.accessToken != presented {
		return
	}
	c.accessToken = ""
	c.tokenExpiry = time.Time{}
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
			// Compare-and-clear, for the same reason invalidateAccessToken uses one:
			// this flight may no longer be the published one. invalidateAccessToken
			// unpublishes a flight whose token a 401 rejected, after which a later
			// caller can publish a NEW flight. An unconditional nil here would erase
			// that newer flight, stranding its waiters on a pointer nobody will ever
			// complete. Only retract this flight if it is still the current one.
			if c.inflight == inflight {
				c.inflight = nil
			}
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
		if inflight.err == nil && inflight.token == "" {
			// The flight succeeded but its result was POISONED by
			// invalidateAccessToken: a 401 rejected exactly this value while the
			// flight was still published. Handing it back would return the token the
			// platform just refused, which is the leak the guard exists to close.
			//
			// An empty token is never an ordinary success — fetchToken rejects an
			// empty access_token before it ever publishes — so this arm cannot fire
			// on a healthy refresh. Re-lead a fresh exchange instead. The leader has
			// already cleared c.inflight by the time done is closed, so the retry
			// starts a NEW flight rather than rejoining this one, and cannot loop on
			// the poisoned result.
			return c.accessTokenValue(ctx)
		}
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

// CustomerID reports the ad account (customer) id this client is bound to. Exposed so a
// caller holding a campaign created under a KNOWN customer can verify the connection it just
// resolved still points at that same account before issuing an account-scoped request —
// campaign ids are unique only WITHIN a customer, so running such a request under a different
// customer reads as "no activity" at best and another account's campaign at worst.
func (c *Client) CustomerID() string { return c.account.CustomerID }

// accessibleResourceNameRE pins the exact two-segment shape AccessibleCustomer promises.
// Anchored on both ends so a value carrying extra path segments cannot pass; the digit
// class is customerIDRE's, kept in step with it deliberately.
var accessibleResourceNameRE = regexp.MustCompile(`^customers/[0-9]+$`)

// validateAccountIDs rejects a CustomerID (and, when set, LoginCustomerID) that
// isn't a digits-only id, before any request is built.
func (c *Client) validateAccountIDs() error {
	if !customerIDRE.MatchString(c.account.CustomerID) {
		return fmt.Errorf("invalid Google Ads customer id %q: must be digits only (no dashes)", c.account.CustomerID)
	}
	return c.validateLoginCustomerID()
}

// validateLoginCustomerID validates ONLY the manager id, for the account-agnostic
// endpoints that have no customer_id path segment (customers:listAccessibleCustomers).
// Those calls are how a caller DISCOVERS a customer id, so requiring one first is the
// chicken-and-egg that made account discovery unreachable. Naming the layer precisely
// matters, because this comment documents which precondition is bypassed: discovery calls
// doRequestValidated, deliberately SKIPPING doRequest's validateAccountIDs. The
// login-customer-id header is attached inside doRequestValidated itself, alongside
// developer-token — so the header is still sent on every discovery call and must still be
// well-formed, which is what this function checks and the only reason it exists separately
// from validateAccountIDs, while c.account.CustomerID is legitimately empty here.
func (c *Client) validateLoginCustomerID() error {
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
	return c.doRequestValidated(ctx, method, path, body, idempotent)
}

// doRequestValidated is doRequest with the account-id precondition already discharged by
// the caller. It exists ONLY so the account-agnostic endpoints — the ones whose whole job
// is to tell a caller which customer ids exist — can run without a customer id, while
// still sharing one copy of the URL construction, header set, body bounding, retry gating,
// and apiError/transportError classification. Every caller must have validated whatever
// ids its path actually embeds first.
func (c *Client) doRequestValidated(ctx context.Context, method, path string, body any, idempotent bool) ([]byte, error) {
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
				// A request HAS already been sent (the 429 proves Google received it), so a
				// cancellation/deadline while waiting to retry leaves the outcome AMBIGUOUS,
				// not "not applied". Returning the bare ctx.Err() here would match neither
				// transportError nor apiError, so createOutcomeAmbiguous would report false
				// and the caller would be told the mutation definitely did not apply. Wrap it
				// so the ambiguity survives. (Harmless on the idempotent GAQL :search path —
				// a read has no commit to be ambiguous about.)
				return nil, &transportError{Method: method, Path: path, Err: err}
			}
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			// The platform rejected this token. Drop it so the next operation re-mints
			// instead of re-presenting it until its advertised expiry.
			//
			// Placed HERE, on the status alone, because three separate exits below return a
			// 401-bearing apiError — unreadable body, oversized body, and the ordinary
			// parsed-body path — and a guard written at any one of them would leave the
			// other two re-presenting the rejected token. This point dominates all three:
			// the status line is known and no 401 can reach a return without passing it.
			// It also means an ambiguous or unparseable 401 is handled exactly like a
			// readable one, which is the case likeliest to accompany a broken auth response.
			c.invalidateAccessToken(token)
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
			raw := buf.Bytes()
			// Parse the machine-readable error codes from the FULL body BEFORE
			// truncating: a real Google error JSON often exceeds maxErrorBodyChars, and
			// parsing the truncated (mid-JSON) snapshot would fail and silently drop the
			// codes — breaking duplicate/field-error classification.
			codes := parseErrorCodes(raw)
			// Slice the BYTES to the cap BEFORE converting to string. Truncating the
			// string after `string(raw)` would keep the 400-char result sharing the full
			// (up-to-maxResponseBytes) backing array, so every retained apiError would
			// pin the whole body. Converting only the bounded slice copies just those
			// bytes, so the snapshot retains at most maxErrorBodyChars.
			snap := raw
			if len(snap) > maxErrorBodyChars {
				snap = snap[:maxErrorBodyChars]
			}
			text := string(snap)
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: text, ErrorCodes: codes}
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
	Results       searchRows `json:"results"`
	NextPageToken pageToken  `json:"nextPageToken"`
}

// searchRows is the repeated `results` field with one added rule: an EXPLICIT JSON null is
// rejected. proto3 JSON emits an empty repeated field as `[]` or omits the key, so
// `{"results":null}` is not a shape a conformant server produces, yet it decodes to a nil
// slice indistinguishable from a genuine empty page — the same false absence the bare-null
// guard below refuses, one level in. An OMITTED key is left alone (UnmarshalJSON is not
// called for it, and `{}` is Google's own empty page).
type searchRows []json.RawMessage

// UnmarshalJSON implements json.Unmarshaler.
func (r *searchRows) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return errors.New("`results` was JSON null, not a result set")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(b, &rows); err != nil {
		return err
	}
	*r = rows
	return nil
}

// pageToken is `nextPageToken` with the same rule searchRows applies to `results`, for the
// same reason and at higher stakes. Decoded as a plain string, an EXPLICIT `null` is
// indistinguishable from an omitted key and from `""` — all three yield "" — and "" is what
// this loop reads as "that was the last page". So a server that answers `{"results":[...],
// "nextPageToken":null}` truncates pagination silently.
//
// That truncation is a FALSE ABSENCE, which is the expensive direction here: FindCampaignByName
// treats "no match in any page" as a licence to CREATE, so a match sitting on page 2 becomes a
// duplicate paid campaign. proto3 JSON omits an unset string field or emits ""; it never emits
// null, so refusing it costs nothing a conformant server would send. An omitted key is left
// alone — UnmarshalJSON is not called for it, and `{}` is Google's own final page.
type pageToken string

// UnmarshalJSON implements json.Unmarshaler.
func (t *pageToken) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return errors.New("`nextPageToken` was JSON null, not a cursor")
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*t = pageToken(s)
	return nil
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
	return c.gaqlSearchForCustomer(ctx, c.account.CustomerID, query)
}

// gaqlSearchForCustomer is gaqlSearch scoped to an EXPLICIT customer id rather than the
// client's configured one. Account discovery needs it: expanding a manager account's
// children runs under the manager id, which is the login-customer-id, at a point where
// c.account.CustomerID is deliberately empty because discovering it is the point of the
// call. The customer id is validated here (not in doRequest) because it is interpolated
// straight into the resource path.
func (c *Client) gaqlSearchForCustomer(ctx context.Context, customerID, query string) ([]json.RawMessage, error) {
	if !customerIDRE.MatchString(customerID) {
		return nil, fmt.Errorf("invalid Google Ads customer id %q: must be digits only (no dashes)", customerID)
	}
	if err := c.validateLoginCustomerID(); err != nil {
		return nil, err
	}
	path := "customers/" + customerID + "/googleAds:search"
	var out []json.RawMessage
	var totalBytes int
	cursor := ""
	seen := map[string]struct{}{}

	for page := 0; page < maxSearchPages; page++ {
		// GAQL search is read-only (idempotent), so a 429 is safe to retry.
		//
		// doRequestValidated, not doRequest: both ids this call depends on were validated
		// at the top of gaqlSearchForCustomer — the customer id because it is interpolated
		// into `path` above, the manager id because it is sent as a header. Re-running
		// doRequest's check would instead validate c.account.CustomerID, which the
		// discovery path deliberately leaves empty.
		raw, err := c.doRequestValidated(ctx, http.MethodPost, path, searchRequest{Query: query, PageToken: cursor}, true /* idempotent */)
		if err != nil {
			return nil, fmt.Errorf("gaql search: %w", err)
		}
		// A top-level JSON `null` unmarshals into searchResponse WITHOUT error and
		// leaves it zero-valued, so it would return a nil row set and no page token —
		// indistinguishable from a genuine empty result. For a caller that reads an
		// empty result as a licence to create, that turns an unverifiable response into
		// a duplicate paid campaign. Google's real empty page is `{}` or
		// `{"results":[]}`, both well-formed objects, so rejecting the bare null costs
		// nothing legitimate.
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, &transportError{Method: http.MethodPost, Path: path, Err: errors.New("search response was a bare JSON null, not a result set")}
		}
		// The row guards in campaign_lookup.go run on rows this envelope has already
		// produced, so they cannot see a corruption that destroys a row on the way out.
		// `{"results":[<campaign 555>],"results":[]}` is the case: encoding/json takes the
		// LAST value, the page decodes to zero rows, and every per-row guard downstream is
		// handed nothing to check. What reaches the caller is a clean, trustworthy absence
		// — the one answer a fail-closed lookup must never manufacture, because its callers
		// read it as a licence to create a real paid campaign. The same applies to the page
		// token: a duplicated `nextPageToken` silently truncates or redirects pagination.
		//
		// So the envelope is checked before it is decoded, with the same guards and for a
		// stronger reason than the rows get them. utf8.Valid and the surrogate scan come
		// along because neither can over-reject: invalid UTF-8 bytes make the document
		// malformed per RFC 8259 §8.1, and Google Ads cannot store an unpaired surrogate in
		// any field, so a page carrying either has already gone wrong upstream. The per-row
		// checks stay where they are — they name the campaign in their diagnostics, which
		// this one cannot.
		if !utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw) {
			return nil, &transportError{Method: http.MethodPost, Path: path, Err: errors.New("search response cannot survive JSON decoding intact (malformed UTF-8 bytes, or an unpaired surrogate escape); decoding it would substitute U+FFFD")}
		}
		if hasDuplicateKeys(raw) {
			return nil, &transportError{Method: http.MethodPost, Path: path, Err: errors.New("search response declares the same JSON key twice; the envelope contradicts itself and its result set cannot be read as an answer, least of all as an empty one")}
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
		if _, dup := seen[string(sr.NextPageToken)]; dup {
			return nil, fmt.Errorf("gaql search %q: server repeated page token — aborting", path)
		}
		seen[string(sr.NextPageToken)] = struct{}{}
		cursor = string(sr.NextPageToken)
	}
	return nil, fmt.Errorf("gaql search %q: exceeded %d pages with a page token still present", path, maxSearchPages)
}

// AccessibleCustomer represents a Google Ads customer (account) accessible via
// the authenticated developer token and login-customer-id (if set). Extracted
// from the ListAccessibleCustomers response.
type AccessibleCustomer struct {
	ResourceName    string
	DescriptiveName string
}

// listAccessibleCustomersResponse is the response from the Google Ads
// customers.listAccessibleCustomers API. The API returns a flat list of the accounts
// the AUTHENTICATED USER can act on directly. The login-customer-id header does not
// scope, filter, or expand this list — see ListAccessibleCustomers below, which walks
// the manager hierarchy separately precisely because this call will not.
type listAccessibleCustomersResponse struct {
	// ResourceNames is the list of customer resources reachable via the credential.
	// Each is formatted as "customers/{customer_id}". This API endpoint — unlike most
	// Google Ads read operations — does NOT require a customer_id path segment;
	// it is accessed at /v{api_version}/customers:listAccessibleCustomers.
	ResourceNames []string `json:"resourceNames"`
}

// ListAccessibleCustomers discovers the ad accounts reachable with this client's
// credential, returning minimal identifying info (resource name and descriptive name).
// The resource name is formatted as "customers/{customer_id}" (digits only, no dashes).
//
// It runs WITHOUT a configured CustomerID — this is the call that tells a caller which
// customer ids exist, so requiring one would be circular — and it is the reason
// doRequestValidated exists.
//
// There are two MODES, and each consults exactly ONE data source — which source answers
// depends entirely on whether a login-customer-id is configured, and the mode is decided
// BEFORE anything goes over the wire. One SOURCE is not one HTTP request: a GAQL read
// pages until its nextPageToken is empty, and either mode retries a 429. The invariant is
// that a mode never issues a request whose response it will not read.
//
// Without one: customers:listAccessibleCustomers stands alone. It gives the accounts the
// authenticated user can act on DIRECTLY, unlabelled (the response carries resource names
// only), and there is no hierarchy root to walk.
//
// With one: the answer is the manager's ENABLED, non-manager children from
// customer_client, and the flat list is not consulted at all. Every other request this
// client makes carries the login-customer-id header, so an account outside that hierarchy
// is not addressable through this client even though the unscoped flat list returns it —
// offering it would present a choice that fails at first dispatch. The expansion is also
// labelled and already has managers filtered out. See the MANAGER MODE comment in the
// body for the full reasoning.
func (c *Client) ListAccessibleCustomers(ctx context.Context) ([]AccessibleCustomer, error) {
	// The login-customer-id header is validated in both modes, before either request.
	if err := c.validateLoginCustomerID(); err != nil {
		return nil, err
	}

	// MANAGER MODE — return before issuing the flat request, not after discarding it.
	//
	// The flat list contributes NOTHING to the manager-mode answer (see the reasoning
	// below), so issuing it would be a second Google Ads round-trip whose result is
	// thrown away. That is not merely wasteful: it spends request quota and whatever
	// deadline the caller passed down, and — the part that actually breaks behaviour —
	// its own timeout, 429, or 5xx propagates and fails the whole discovery even when
	// the hierarchy query alone would have answered correctly. A call whose success is
	// not needed must not be able to cause a failure.
	//
	// This is why the mode branch sits here rather than after the flat read: the
	// ordering IS the fix. An earlier revision validated flat-list resource names in
	// both modes "regardless of which branch consumes them"; in manager mode those rows
	// are never consumed, so that validation only supplied one more way to fail. The
	// manager-mode rows get their own id validation inside listManagerClients.
	if c.account.LoginCustomerID != "" {
		return c.expandManagerHierarchy(ctx)
	}

	// DIRECT MODE. The flat list is the answer.
	//
	// The ListAccessibleCustomers endpoint is account-agnostic; it does NOT have a
	// customer_id path segment. The path is just customers:listAccessibleCustomers.
	//
	// The REST binding for CustomerService.ListAccessibleCustomers is GET, not the
	// POST used by the :search and :mutate custom methods — it takes no request
	// body at all. Sending POST here fails against the real API.
	//
	// This goes through the SHARED request layer (doRequestValidated, which doRequest
	// itself wraps): the URL construction, header set, body bounding, and
	// apiError/transportError classification are identical, so duplicating them here
	// would be a second copy to keep in sync. The one thing it deliberately bypasses is
	// doRequest's digits-only CustomerID precondition: doRequest insists on a
	// digits-only c.account.CustomerID, and this call is how a caller LEARNS one.
	// Requiring an account id before account discovery is the chicken-and-egg that made
	// the endpoint unreachable for a connection that has credentials but no account
	// chosen yet. The nil body means the layer omits Content-Type, and idempotent=true
	// is correct because this is a pure read: retrying a 429 cannot double-apply
	// anything, which is exactly the case the shared retry is gated on.
	const path = "customers:listAccessibleCustomers"

	raw, err := c.doRequestValidated(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}

	var resp2xx listAccessibleCustomersResponse
	if err := json.Unmarshal(raw, &resp2xx); err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("decode response: %w", err)}
	}

	// Convert resource names to AccessibleCustomer structs. This endpoint returns
	// resource_names ONLY — no descriptive_name and no `manager` flag — so labels stay
	// empty here, and a manager account in this list is indistinguishable from an ad
	// account by its resource name alone.
	//
	// The AccessibleCustomer contract promises "customers/{digits}", a caller persists
	// this value as the connection's account id and interpolates it into later request
	// paths, and a malformed 2xx must not put an empty, wrong-kind, or path-bearing
	// string on that route. Fail the read rather than hand back an account id that is
	// unusable at best.
	direct := make([]AccessibleCustomer, 0, len(resp2xx.ResourceNames))
	seen := make(map[string]struct{}, len(resp2xx.ResourceNames))
	for _, resName := range resp2xx.ResourceNames {
		if !accessibleResourceNameRE.MatchString(resName) {
			return nil, &transportError{
				Method: http.MethodGet,
				Path:   path,
				Err:    fmt.Errorf("resource name %q is not of the form customers/{digits}", resName),
			}
		}
		if _, dup := seen[resName]; dup {
			continue
		}
		seen[resName] = struct{}{}
		direct = append(direct, AccessibleCustomer{ResourceName: resName})
	}

	// The direct list IS the answer: every account in it is one the credential addresses
	// on its own behalf, and there is no hierarchy root to walk. (A manager account can
	// still be in here and cannot hold campaigns, but with no `manager` flag on this
	// response there is nothing to recognise it by, and a round-trip per row to find out
	// would cost more than it saves on a list this short.)
	return direct, nil
}

// expandManagerHierarchy answers account discovery for a client configured with a manager
// (MCC) login-customer-id. It is the whole of manager mode: the flat
// customers:listAccessibleCustomers response is never fetched, because it is not a merge
// and that is the whole point.
//
// listAccessibleCustomers is explicitly UNSCOPED — the login-customer-id header does not
// filter it, and it does not walk a hierarchy either. Every OTHER request this client
// makes does carry that header, so an account is only addressable through this client if
// it sits under the configured manager. An account the user reaches directly but which
// belongs to a different hierarchy therefore comes back in the flat list and then fails
// with PERMISSION_DENIED the moment anything is done with it. Returning it presents the
// caller a choice that cannot work, and the failure lands at first dispatch — long after
// the connection was saved — where it reads as a credential problem rather than a
// wrong-account one.
//
// So in manager mode the selectable set is exactly the manager's ENABLED, non-manager
// children. That set is also strictly better shaped: customer_client carries
// descriptive_name, so those accounts come back labelled, and listManagerClients has
// already dropped the managers the flat list cannot identify. Any account present in both
// lists is in the expansion too, so nothing addressable is lost — which is precisely why
// fetching the flat list first would have been pure cost.
func (c *Client) expandManagerHierarchy(ctx context.Context) ([]AccessibleCustomer, error) {
	children, cerr := c.listManagerClients(ctx, c.account.LoginCustomerID)
	if cerr != nil {
		return nil, cerr
	}
	accounts := make([]AccessibleCustomer, 0, len(children))
	at := make(map[string]int, len(children))
	for _, child := range children {
		// customer_client can report the same client twice when the hierarchy has more
		// than one path to it (a sub-manager that is itself a client of the root).
		//
		// Keep the FIRST occurrence, and note what that does and does not rest on. The
		// query selects no customer_client.level and carries no ORDER BY, so GAQL makes no
		// promise about which path is returned first — an earlier version of this comment
		// claimed the first was "the shallowest", which is not an invariant the query
		// provides and would have been a trap for anyone later relying on depth. It does
		// not matter here: every duplicate describes the SAME customer, so id and
		// descriptive_name are properties of that customer rather than of the path taken
		// to reach it, and the two rows are interchangeable in the only two fields kept.
		//
		// The one asymmetry worth handling is a blank label. If a later duplicate carries
		// a descriptive_name the first lacked, take it — a labelled account is strictly
		// more useful in a picker than an unlabelled one, and this makes the result
		// independent of arrival order rather than merely tolerant of it.
		if i, dup := at[child.ResourceName]; dup {
			if accounts[i].DescriptiveName == "" {
				accounts[i].DescriptiveName = child.DescriptiveName
			}
			continue
		}
		at[child.ResourceName] = len(accounts)
		accounts = append(accounts, child)
	}

	return accounts, nil
}

// customerClientRow is one customer_client row from the manager-hierarchy expansion.
type customerClientRow struct {
	CustomerClient struct {
		ID              string `json:"id"`
		DescriptiveName string `json:"descriptiveName"`
		Manager         bool   `json:"manager"`
		Status          string `json:"status"`
	} `json:"customerClient"`
}

// listManagerClients enumerates the ad accounts beneath a manager (MCC) account.
//
// Manager accounts are excluded from the result: they cannot hold campaigns, so offering
// one as a choice would let a caller select an account that fails at the first create.
// Only ENABLED clients are requested — a cancelled or closed account is not somewhere a
// campaign can run either.
func (c *Client) listManagerClients(ctx context.Context, managerID string) ([]AccessibleCustomer, error) {
	const query = `SELECT customer_client.id, customer_client.descriptive_name, ` +
		`customer_client.manager, customer_client.status FROM customer_client ` +
		`WHERE customer_client.status = 'ENABLED'`

	rows, err := c.gaqlSearchForCustomer(ctx, managerID, query)
	if err != nil {
		return nil, fmt.Errorf("expand manager %s: %w", managerID, err)
	}

	out := make([]AccessibleCustomer, 0, len(rows))
	for _, raw := range rows {
		var row customerClientRow
		if uerr := json.Unmarshal(raw, &row); uerr != nil {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   "customers/" + managerID + "/googleAds:search",
				Err:    fmt.Errorf("decode customer_client row: %w", uerr),
			}
		}
		// A row whose id is missing OR non-numeric is unusable: AccessibleCustomer
		// promises a digits-only customer id, and this id is concatenated straight into
		// "customers/"+id. An empty check alone lets a value like "1/other" through and
		// forges a resource name pointing somewhere else entirely. Silently dropping such
		// a row would understate the list, so fail the read.
		if !customerIDRE.MatchString(row.CustomerClient.ID) {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   "customers/" + managerID + "/googleAds:search",
				Err:    fmt.Errorf("customer_client row id %q is not a numeric customer id", row.CustomerClient.ID),
			}
		}
		if row.CustomerClient.Manager {
			continue
		}
		out = append(out, AccessibleCustomer{
			ResourceName:    "customers/" + row.CustomerClient.ID,
			DescriptiveName: row.CustomerClient.DescriptiveName,
		})
	}
	return out, nil
}
