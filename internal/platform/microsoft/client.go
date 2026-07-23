// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package microsoft provides a Go client for the Microsoft Advertising
// (Bing Ads) Campaign Management REST API.
//
// It speaks the Microsoft Advertising REST transport directly (v13):
//
//   - mutations via POST CampaignManagement/v13/Campaigns
//
// REST (rather than the legacy SOAP Bulk/Campaign Management service) is used
// deliberately so this client matches the meta/reddit/twitter/linkedin/googleads
// clients' structure and avoids a SOAP dependency.
//
// Like the Google Ads client, Microsoft Advertising auth requires an OAuth2
// refresh-token exchange (against the Microsoft identity platform) plus a
// developer token and account/customer-id headers on every call. Credentials and
// account configuration are injected via NewClient; the client never reads the
// process environment.
//
// This file is the client scaffold: auth, the request layer. Campaign creation
// lands in campaign.go.
//
// Naming note: the platform key surfaced to callers (CampaignResult.Platform, every
// error prefix) is "microsoft-ads", even though the live REST host is the legacy
// campaign.api.bingads.microsoft.com domain — "Bing Ads" and "Microsoft Advertising"
// are the same platform.
package microsoft

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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Microsoft Advertising API constants
// ---------------------------------------------------------------------------

const (
	// msAdsAPIVersion is the Campaign Management API version segment in the URL
	// path. Microsoft supports a small number of concurrent versions; bump
	// deliberately and re-verify the entity field set when doing so.
	msAdsAPIVersion = "v13"

	// msAdsBaseURL is the Microsoft Advertising REST base (version appended per
	// request). The Campaign Management service is hosted under /CampaignManagement.
	msAdsBaseURL = "https://campaign.api.bingads.microsoft.com"

	// msOAuthTokenURL is the Microsoft identity platform OAuth 2.0 token endpoint
	// used to exchange a refresh token for a short-lived access token. The "common"
	// tenant serves both work/school and personal Microsoft accounts, which is what
	// Microsoft Advertising app registrations use.
	msOAuthTokenURL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"

	// msAdsScope is the OAuth scope required for the Microsoft Advertising API. The
	// offline_access scope is what mints the refresh token; msads.manage is the API
	// permission. Sent on the refresh exchange so the returned access token carries
	// the advertising audience.
	msAdsScope = "https://ads.microsoft.com/msads.manage offline_access"

	// msAdsRequestTimeout bounds a single API call.
	msAdsRequestTimeout = 30 * time.Second

	// tokenExpiryBuffer refreshes the access token this long before its stated
	// expiry so an in-flight request is not made with a just-expired token.
	tokenExpiryBuffer = 60 * time.Second

	// defaultTokenTTL is the fallback token lifetime used when the OAuth response
	// omits (or reports a non-positive) expires_in, so a valid-but-lifetimeless
	// token still works without caching an already-expired entry.
	defaultTokenTTL = 30 * time.Minute

	// maxResponseBytes bounds a response body read so an unexpectedly large
	// response cannot exhaust memory. Mirrors the sibling clients.
	maxResponseBytes = 8 << 20 // 8 MiB

	// maxErrorBodyChars caps the length (in runes) of an untrusted string embedded in
	// an error message — currently a redacted destination URL (see redactAdURL) — so a
	// pathologically long value can't bloat a persisted campaign step. It is NOT an
	// error-body snapshot: a non-2xx body is never retained (only its parsed error
	// codes are; see apiError).
	maxErrorBodyChars = 400

	// retryMax is the number of times an HTTP 429 (rate-limited) IDEMPOTENT
	// request is retried before giving up. Mirrors the sibling clients.
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
// Microsoft Advertising API. All are injected (never read from the environment).
//
// Which of these are stored encrypted vs as plain provider-config is a
// connection-layer concern; this client treats them all as injected inputs.
type Credentials struct {
	// ClientID / ClientSecret identify the OAuth2 (Microsoft identity platform)
	// application.
	ClientID     string
	ClientSecret string
	// DeveloperToken is the Microsoft Advertising developer token, sent as the
	// `DeveloperToken` header on every call.
	DeveloperToken string
	// RefreshToken is exchanged for a short-lived access token.
	RefreshToken string
}

// AccountConfig identifies the ad account the client operates on.
type AccountConfig struct {
	// AccountID is the Microsoft Advertising ad account id, DIGITS ONLY, e.g.
	// "1234567". Sent as the `CustomerAccountId` header on every call.
	AccountID string
	// CustomerID is the OPTIONAL parent customer (manager) id, DIGITS ONLY. Sent as
	// the `CustomerId` header when set. Empty omits the header (single-account
	// access derives the customer server-side).
	CustomerID string
	// Label is an optional human-readable account label surfaced in results.
	Label string
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client is a Microsoft Advertising API client for one ad account.
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
	// cache empty/expired becomes the leader: it publishes a *tokenRefresh here and
	// runs the fetch on a detached context in a goroutine. Followers wait on the
	// shared tokenRefresh.done channel and read the shared result, so one caller's
	// cancellation can't tear down a refresh the others depend on, and a failed
	// refresh fails all current waiters at once rather than each re-leading a serial
	// refresh. Mirrors the google-ads client.
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
// the sibling clients' noFollow.
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

// WithAPIVersion overrides the Campaign Management API version segment. Lets a
// deployment pin/bump the version without a code change, and lets tests assert
// the version reaches the path.
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

// NewClient builds a Microsoft Advertising client from injected credentials and
// account config. Redirect following is force-disabled on whatever *http.Client is
// used, including one supplied via WithHTTPClient (applied to a shallow copy so the
// caller's client is not mutated). Mirrors the google-ads/reddit clients.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		account:        account,
		baseURL:        msAdsBaseURL,
		apiVersion:     msAdsAPIVersion,
		tokenURL:       msOAuthTokenURL,
		httpClient:     &http.Client{Timeout: msAdsRequestTimeout, CheckRedirect: noFollow},
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
// Error types (mirror the meta/reddit/google-ads ambiguity contract)
// ---------------------------------------------------------------------------

// apiError is a non-2xx response from the Microsoft Advertising API (the OAuth
// token exchange never produces an apiError — its failures are plain errors that
// never echo the token-endpoint body). It carries status/method/path so an error
// names exactly which call failed. Only the parsed machine-readable ErrorCodes are
// kept; the raw upstream body is dropped after code extraction (see below).
type apiError struct {
	StatusCode int
	Method     string
	Path       string
	// ErrorCodes holds Microsoft's machine-readable error codes, parsed from the FULL
	// error body in doRequest. This is what hasErrorCode matches on, so duplicate/
	// field-error detection works even for error payloads longer than any snapshot cap.
	//
	// The raw error body is deliberately NOT retained: it is untrusted upstream text
	// that can reflect request/credential material, and an exported field would be
	// walked by JSON/reflection-based logging of the error — bypassing the status-only
	// Error() and recreating the exact leak channel transportError's unexported cause
	// closes. Classification needs only the parsed codes, so the body is dropped after
	// codes are extracted.
	ErrorCodes []string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("microsoft-ads %s %s -> %d", e.Method, e.Path, e.StatusCode)
}

// transportError wraps a round-trip failure that happened AFTER the request was
// plausibly sent (mid-flight timeout, EOF, reset), OR a failure to read a 2xx
// response: the server may or may not have processed the request, so the outcome is
// AMBIGUOUS. A pre-send failure (request build, pre-connect dial — see
// isPreSendDialError) is NOT wrapped as transportError. Mirrors the sibling clients.
//
// The wrapped cause is UNEXPORTED (`err`): the cause is typically a *url.Error whose
// EXPORTED URL field carries the full request URL. Error() strips it via safeCause,
// but an exported field would let reflection/JSON serialization of the error (a
// structured logger, error middleware) walk into that URL and leak it into a
// persisted campaign step. Keeping it unexported closes that walk while Unwrap()
// still exposes the cause for errors.Is/As. Mirrors the hubspot/twitter clients.
type transportError struct {
	Method string
	Path   string
	err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("microsoft-ads %s %s: %s", e.Method, e.Path, safeCause(e.err))
}

func (e *transportError) Unwrap() error { return e.err }

// safeCause renders a URL-free description of a round-trip error. Peeling a
// *url.Error (whose %v embeds the request URL) is not sufficient on its own: because
// WithHTTPClient accepts a custom http.RoundTripper, the INNER error text is
// caller-controlled and can itself embed the URL. So this NEVER echoes an unknown
// error's text — it maps to a fixed vocabulary of URL-free descriptions and falls
// back to a generic "transport failure" for anything it doesn't recognize. Mirrors
// the hubspot client's safeCause.
func safeCause(err error) string {
	if err == nil {
		return "transport failure"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context deadline exceeded"
	}
	// A timeout (i/o timeout, TLS handshake timeout, per-attempt deadline) — emit our
	// OWN fixed string, never the error's text; errors.As reaches a net.Error even
	// through a *url.Error wrapper.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection closed"
	}
	// Default-deny: unknown error text is NEVER rendered.
	return "transport failure"
}

// isPreSendDialError reports whether a httpClient.Do error clearly happened
// BEFORE the request could be sent — a DNS resolution failure or a connect-time
// dial failure (connection refused / no route / network unreachable). Only these
// prove the request never reached Microsoft, so a mutation definitely did not
// happen. Every other Do error (mid-flight timeout, EOF, reset) is ambiguous.
// Mirrors the sibling clients.
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
// (mirrors the google-ads/reddit clients). The lock is NOT held across the network
// call: the fast path reads the cache under a brief lock, and every waiter (leader
// included) selects on its own ctx so a cancelled caller returns promptly with its
// context error instead of blocking on — or tearing down — the shared refresh. A
// failed refresh fails all current waiters at once rather than each re-leading a
// serial refresh (which would amplify rate-limit pressure).
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
		// request-scoped VALUES via context.WithoutCancel. No lock is held across the
		// network call.
		inflight = &tokenRefresh{done: make(chan struct{})}
		c.inflight = inflight
		refreshValuesCtx := context.WithoutCancel(ctx)
		go func() {
			fetchCtx, cancel := context.WithTimeout(refreshValuesCtx, msAdsRequestTimeout)
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
// result under tokenMu. It is only ever invoked from the leader's detached refresh
// goroutine, so at most one call is in flight at a time.
func (c *Client) fetchToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("client_id", c.creds.ClientID)
	form.Set("client_secret", c.creds.ClientSecret)
	form.Set("refresh_token", c.creds.RefreshToken)
	form.Set("grant_type", "refresh_token")
	form.Set("scope", msAdsScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A token-endpoint failure ran no mutation; surface it plainly (the caller
		// aborts before any create). Do NOT wrap as transportError.
		return "", fmt.Errorf("microsoft-ads token refresh: %w", err)
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
		return "", fmt.Errorf("microsoft-ads token refresh -> %d", resp.StatusCode)
	}

	var tok tokenResponse
	if err := json.Unmarshal(buf.Bytes(), &tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("microsoft-ads token refresh returned an empty access_token")
	}

	// expires_in may be absent; default so a missing value doesn't pin a stale token
	// forever (nor cache an already-expired entry).
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

// accountIDRE matches a Microsoft Advertising account/customer id: digits only.
// The connection's account_id is user-supplied and its Goa design only checks
// presence, so it must be validated here before being placed in a header — a
// padded/dashed id yields an invalid request, and control characters could inject
// a header. Mirrors the google-ads customerIDRE.
var accountIDRE = regexp.MustCompile(`^[0-9]+$`)

// validateAccountIDs rejects an AccountID (and, when set, CustomerID) that isn't a
// digits-only id, before any request is built.
func (c *Client) validateAccountIDs() error {
	if !accountIDRE.MatchString(c.account.AccountID) {
		return fmt.Errorf("invalid Microsoft Advertising account id %q: must be digits only", c.account.AccountID)
	}
	if c.account.CustomerID != "" && !accountIDRE.MatchString(c.account.CustomerID) {
		return fmt.Errorf("invalid Microsoft Advertising customer id %q: must be digits only", c.account.CustomerID)
	}
	return nil
}

// doRequest performs one Microsoft Advertising REST call against
// {baseURL}/CampaignManagement/{version}/{path}, attaching the bearer access
// token, DeveloperToken, CustomerAccountId, and (when set) CustomerId headers.
// body is JSON-encoded when non-nil. It returns the raw 2xx body bytes; non-2xx
// and transport failures are classified per the ambiguity contract.
//
// idempotent gates 429 retry behavior. A rate-limited IDEMPOTENT call (a read) is
// retried up to retryMax times with a bounded backoff honoring Retry-After. A
// NON-idempotent call (a Campaigns POST that creates a paid resource) is NOT
// retried: the create endpoint has no idempotency key, so a 429 whose first
// attempt may already have committed upstream would double-create on retry. For
// those the 429 is returned as an apiError immediately (and createOutcomeAmbiguous
// treats a mutating 429 as "may exist").
func (c *Client) doRequest(ctx context.Context, method, path string, body any, idempotent bool) ([]byte, error) {
	// Validate the account/customer ids at the shared request choke point (mirrors the
	// google-ads client). They flow into the CustomerAccountId/CustomerId request
	// HEADERS below, and validateAccountIDs is what keeps a control char out of a
	// header; doing it here (not only in CreateCampaign) covers every future caller
	// that routes through doRequest — e.g. a later read/metrics helper.
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}

	var payload []byte
	if body != nil {
		p, err := json.Marshal(body)
		if err != nil {
			// A marshal failure is a pre-send programmer/input error — nothing was sent.
			return nil, fmt.Errorf("microsoft-ads %s %s: encode request: %w", method, path, err)
		}
		payload = p
	}

	fullURL := c.baseURL + "/CampaignManagement/" + c.apiVersion + "/" + path

	for attempt := 0; ; attempt++ {
		// Fetch the token INSIDE the loop: after a 429 backoff (up to maxRetryWait per
		// attempt) a token cached before the loop could have expired, so a resumed retry
		// would 401. accessTokenValue returns the cached token on the fast path, so this
		// is nearly free on the first attempt. Mirrors the google-ads sibling.
		token, err := c.accessTokenValue(ctx)
		if err != nil {
			return nil, err
		}

		raw, retryAfter, retryable, aerr := c.attempt(ctx, method, fullURL, path, token, payload)
		// retryable is set only for a 429. It is retried ONLY when the call is
		// idempotent AND attempts remain — a non-idempotent 429 (a create that may
		// have committed) is returned immediately so a blind retry can't double-create.
		// On the final allowed attempt the 429's apiError (aerr) is returned so the
		// caller still sees a rate-limit outcome rather than a bare nil.
		if !retryable || !idempotent || attempt >= retryMax {
			return raw, aerr
		}
		// A server-DECLARED reset longer than maxRetryWait is a signal to ABORT, not
		// clamp: sleeping only maxRetryWait can't clear the window, so a clamped retry
		// would just 429 again and burn attempts (up to retryMax*maxRetryWait of wall
		// time) while holding the caller — likely past the ingress read timeout. On the
		// over-cap sentinel, surface the 429 apiError immediately so the caller gets a
		// clean rate-limit signal. (Only the parsed Retry-After can be over-cap; the
		// exponential-backoff fallback is already bounded by backoff().) Mirrors google-ads.
		if retryAfter == overCapRetryAfter {
			return nil, aerr
		}
		wait := retryAfter
		if wait <= 0 {
			wait = c.backoff(attempt)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// attempt performs one HTTP round-trip. It returns:
//   - raw: the 2xx body (on success),
//   - retryAfter: a server-declared wait when the status is a 429,
//   - retryable: true ONLY for a 429 (the one status the caller may retry, and only
//     for an idempotent call); false for success, definite failure, and transport
//     errors — the caller stops on those regardless.
//   - err: the classified error on a non-2xx/transport outcome.
//
// Splitting the per-attempt work out keeps the retry loop readable and ensures the
// response body is always closed via defer on every exit path.
func (c *Client) attempt(ctx context.Context, method, fullURL, path, token string, payload []byte) (raw []byte, retryAfter time.Duration, retryable bool, err error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	// Bound EACH attempt with a per-call timeout derived from the caller context, so a
	// custom httpClient supplied via WithHTTPClient with Timeout==0 can't hang
	// indefinitely (the default client sets msAdsRequestTimeout, but a caller's may
	// not). The caller context stays the parent so a real cancel/deadline still
	// propagates. cancel() runs on every exit path. Mirrors the google-ads client.
	attemptCtx, cancel := context.WithTimeout(ctx, msAdsRequestTimeout)
	defer cancel()

	req, rerr := http.NewRequestWithContext(attemptCtx, method, fullURL, bodyReader)
	if rerr != nil {
		// Building the request is a pre-send failure — nothing was sent. Do NOT
		// classify as ambiguous.
		return nil, 0, false, fmt.Errorf("microsoft-ads %s %s: build request: %w", method, path, rerr)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("DeveloperToken", c.creds.DeveloperToken)
	req.Header.Set("CustomerAccountId", c.account.AccountID)
	if c.account.CustomerID != "" {
		req.Header.Set("CustomerId", c.account.CustomerID)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, derr := c.httpClient.Do(req)
	if derr != nil {
		if isPreSendDialError(derr) {
			// The request never left the host: a mutation definitely did not happen.
			return nil, 0, false, fmt.Errorf("microsoft-ads %s %s: %s", method, path, safeCause(derr))
		}
		// Ambiguous: the request may have been received before the failure.
		return nil, 0, false, &transportError{Method: method, Path: path, err: derr}
	}
	defer func() { _ = resp.Body.Close() }()

	buf := new(bytes.Buffer)
	if _, rerr := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); rerr != nil {
		// Status is already known. Preserve it: a read failure on a 2xx is ambiguous
		// (the mutation may have committed but we can't read the result), so it is a
		// transportError; a read failure on a KNOWN non-2xx keeps its status as an
		// apiError so definite-4xx / 429-retry classification is not lost. Crucially, a
		// 429 whose body read fails must STILL follow the retry path (with its
		// Retry-After) — an idempotent call would otherwise skip the bounded retries the
		// status line clearly warrants — so this uses the same retryAfterIf429/
		// is429Status signalling as the oversize path below. Mirrors the siblings.
		return nil, c.retryAfterIf429(resp), c.is429Status(resp.StatusCode),
			c.statusAwareReadError(resp.StatusCode, method, path, rerr)
	}
	if int64(buf.Len()) > maxResponseBytes {
		// Same status-aware discipline for an oversized body: an oversized 2xx create
		// may have committed (ambiguous → transportError), while an oversized non-2xx
		// keeps its status so a 5xx/429 stays ambiguous-via-status and a 4xx stays
		// definite. A plain fmt.Errorf here would be neither, letting createOutcome-
		// Ambiguous return false and inviting a duplicate create.
		return nil, c.retryAfterIf429(resp), c.is429Status(resp.StatusCode),
			c.statusAwareReadError(resp.StatusCode, method, path, fmt.Errorf("response exceeds %d bytes", maxResponseBytes))
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return buf.Bytes(), 0, false, nil
	}

	// Non-2xx: build a classified apiError. Parse error codes from the FULL body; the
	// raw body itself is NOT retained (untrusted; would be a reflection/JSON leak
	// channel — see apiError). Classification uses only the parsed codes.
	ae := &apiError{
		StatusCode: resp.StatusCode,
		Method:     method,
		Path:       path,
		ErrorCodes: parseErrorCodes(buf.Bytes()),
	}

	// A 429 is the one retryable status: signal it with retryable=true + a
	// server-declared Retry-After. doRequest decides whether to ACTUALLY retry (only
	// for an idempotent call with attempts remaining) — a non-idempotent 429 is
	// returned to the caller as an ambiguous apiError instead.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, c.parseRetryAfter(resp), true, ae
	}
	return nil, 0, false, ae
}

// statusAwareReadError builds the right error type for a body read/oversize failure
// where the HTTP status is ALREADY known. A 2xx (the mutation may have committed but
// the result is unreadable) is AMBIGUOUS → transportError; a known non-2xx keeps its
// status as an apiError so definite-4xx and 429-retry classification survive. The
// cause is never surfaced verbatim (transportError uses safeCause; apiError.Error()
// omits the body). Mirrors the google-ads read-failure handling.
func (c *Client) statusAwareReadError(status int, method, path string, cause error) error {
	if status >= 200 && status < 300 {
		return &transportError{Method: method, Path: path, err: cause}
	}
	return &apiError{StatusCode: status, Method: method, Path: path}
}

// retryAfterIf429 returns the parsed Retry-After when the response is a 429 (so a
// body-unreadable / oversized 429 still follows the retry/over-cap-abort path), or 0
// otherwise. Used by BOTH the read-failure and oversize paths, where the body can't be
// consumed but the 429 status line still warrants a bounded retry.
func (c *Client) retryAfterIf429(resp *http.Response) time.Duration {
	if resp.StatusCode == http.StatusTooManyRequests {
		return c.parseRetryAfter(resp)
	}
	return 0
}

// is429Status reports whether status is the one retryable status (429), so the caller
// can retry a body-unreadable/oversized 429 rather than treating it as terminal.
func (c *Client) is429Status(status int) bool {
	return status == http.StatusTooManyRequests
}

// backoff returns the exponential backoff for a 429 retry attempt (0-indexed):
// retryBaseDelay * 2^attempt, capped by maxRetryWait at the call site.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.retryBaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
		if d > maxRetryWait {
			return maxRetryWait
		}
	}
	return d
}

// overCapRetryAfter is a sentinel (strictly greater than any legitimate clamped
// wait) that parseRetryAfter returns when the server-declared Retry-After exceeds
// maxRetryWait. doRequest treats it as abort-don't-retry: a wait that long can't be
// satisfied within the retry budget, so retrying would only 429 again. It is a
// distinct value (maxRetryWait+time.Second) rather than an in-range duration so
// doRequest can detect it unambiguously.
const overCapRetryAfter = maxRetryWait + time.Second

// parseRetryAfter reads a Retry-After header (delta-seconds or an HTTP-date) and
// returns the wait duration, clamped to non-negative. Returns 0 when absent or
// unparseable, so the caller falls back to exponential backoff. A parsed wait that
// EXCEEDS maxRetryWait returns the overCapRetryAfter sentinel, signalling doRequest
// to abort rather than clamp-and-retry into a guaranteed second 429. Mirrors the
// google-ads sibling.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	// delta-seconds form. Compare in SECONDS before converting to Duration: a huge
	// value (e.g. 9223372036854775807) would overflow `secs * time.Second` and wrap to
	// a non-positive Duration, silently skipping the over-cap abort and triggering an
	// ordinary retry. maxRetryWaitSeconds is the cap expressed in seconds so the
	// comparison never multiplies.
	if secs, err := parseNonNegativeInt(v); err == nil {
		if secs > maxRetryWaitSeconds {
			return overCapRetryAfter
		}
		return time.Duration(secs) * time.Second
	} else if isAllDigits(v) {
		// An all-digit value that failed to parse is a delta-seconds value that
		// OVERFLOWED int64 — i.e. an enormous reset far beyond maxRetryWait. Treat it as
		// over-cap (abort), NOT as an unparseable 0 that would fall through to ordinary
		// backoff-and-retry. (parseNonNegativeInt only rejects an all-digit value on
		// overflow; anything non-digit takes the HTTP-date path below.)
		return overCapRetryAfter
	}
	// HTTP-date form.
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return capRetryAfter(d)
		}
	}
	return 0
}

// isAllDigits reports whether s is non-empty and consists only of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// maxRetryWaitSeconds is maxRetryWait expressed in whole seconds, for comparing a
// delta-seconds Retry-After BEFORE converting to a (potentially overflowing) Duration.
const maxRetryWaitSeconds = int64(maxRetryWait / time.Second)

// capRetryAfter returns d unchanged when it is within maxRetryWait, or the
// overCapRetryAfter sentinel when it exceeds it (so doRequest aborts). Used only for
// the HTTP-date form, where d is a difference of two bounded times and cannot
// overflow; the delta-seconds form compares in seconds before converting.
func capRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryWait {
		return overCapRetryAfter
	}
	return d
}

// ---------------------------------------------------------------------------
// Error-code parsing + classification (mirror the google-ads/twitter contract)
// ---------------------------------------------------------------------------

const (
	// maxRetainedErrorCodes / maxErrorCodeLen bound how many/how-long error codes
	// are retained from a response body: they are used only for enum classification,
	// never surfaced, so bounding them keeps a hostile body from bloating even the
	// retained apiError.
	maxRetainedErrorCodes = 16
	maxErrorCodeLen       = 128
)

// msErrorEnvelope is the shape of a Microsoft Advertising REST error body. The
// service returns errors two ways: a top-level ApiFaultDetail fault object (non-2xx),
// and — on a 200 with per-entity failures — a PartialErrors array (handled in
// campaign.go). The v13 ApiFaultDetail schema splits its per-item faults into TWO
// arrays: OperationErrors (request-level) and BatchErrors (per-list-item); a code
// present only in BatchErrors would be missed if we visited OperationErrors alone.
// This envelope captures the machine-readable Code/ErrorCode from the top-level error
// AND any nested Errors/OperationErrors/BatchErrors/PartialErrors, so classification
// works regardless of which array the service used. Message/Details are intentionally
// NOT captured — only the codes are retained, and only for internal classification.
type msErrorEnvelope struct {
	// Top-level operation error (non-2xx bodies).
	Code      json.RawMessage `json:"Code"`
	ErrorCode json.RawMessage `json:"ErrorCode"`
	// Some responses nest the operation errors under an Errors/OperationErrors array.
	Errors          []msErrorItem `json:"Errors"`
	OperationErrors []msErrorItem `json:"OperationErrors"`
	// BatchErrors is the ApiFaultDetail per-list-item fault array (v13). A duplicate/
	// field error on one item of a batch mutate lands here, not in OperationErrors.
	BatchErrors []msErrorItem `json:"BatchErrors"`
	// PartialErrors is present on a 200 that had per-entity failures.
	PartialErrors []msErrorItem `json:"PartialErrors"`
}

// msErrorItem is one error entry (a v13 BatchError/OperationError). Microsoft uses
// both a numeric Code and a string ErrorCode across services; capture both as raw and
// normalize to a string.
type msErrorItem struct {
	Code      json.RawMessage `json:"Code"`
	ErrorCode json.RawMessage `json:"ErrorCode"`
}

// parseErrorCodes extracts Microsoft's machine-readable error codes (string
// ErrorCode enums, e.g. "CampaignServiceEditorialError", or a stringified numeric
// Code) from a non-2xx body. Over-long values and codes beyond the cap are dropped.
// Returns nil on a malformed/absent body. Mirrors the google-ads parseErrorCodes.
func parseErrorCodes(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var env msErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	var codes []string
	add := func(raw json.RawMessage) bool {
		v := codeString(raw)
		if v == "" || len(v) > maxErrorCodeLen {
			return true // skip, keep going
		}
		codes = append(codes, v)
		return len(codes) < maxRetainedErrorCodes
	}
	if !add(env.ErrorCode) || !add(env.Code) {
		return codes
	}
	for _, group := range [][]msErrorItem{env.Errors, env.OperationErrors, env.BatchErrors, env.PartialErrors} {
		for _, it := range group {
			if !add(it.ErrorCode) || !add(it.Code) {
				return codes
			}
		}
	}
	return codes
}

// codeString normalizes a Code/ErrorCode raw value to a string, accepting either a
// JSON string ("SomeErrorCode") or a JSON number (509) — Microsoft uses both.
func codeString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// hasErrorCode reports whether the apiError carried the given Microsoft error code
// (string enum or stringified numeric). It reads the ErrorCodes parsed from the FULL
// body in doRequest, so classification works even for large error payloads.
func (e *apiError) hasErrorCode(code string) bool {
	for _, c := range e.ErrorCodes {
		if strings.EqualFold(c, code) {
			return true
		}
	}
	return false
}

// isMutatingMethod reports whether an HTTP method can create/modify server state,
// so a 3xx on it may hide a committed mutation. Mirrors the sibling clients.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// isDefiniteClientError reports whether ae is a definite 4xx client-error rejection
// that is NOT ambiguous — i.e. a 4xx EXCEPT 429. A 429 is excluded because
// createOutcomeAmbiguous classifies a mutating 429 as possibly-committed (doRequest
// does not retry a non-idempotent 429), so a code carried on a 429 must not be read
// as a definite rejection. Mirrors the google-ads client.
func isDefiniteClientError(ae *apiError) bool {
	return ae.StatusCode >= 400 && ae.StatusCode < 500 &&
		ae.StatusCode != http.StatusTooManyRequests
}

// createOutcomeAmbiguous reports whether a failed MUTATING request MAY have been
// committed upstream (so a caller must reconcile/verify before retrying, to avoid a
// duplicate — the create endpoint has no idempotency key). A 5xx apiError or any
// transportError is ambiguous regardless of method; a mutating 429 is ambiguous
// (doRequest does not retry a non-idempotent 429 precisely because it may have
// committed); a 3xx is ambiguous only on a mutating method. A definite 4xx and a
// pre-send error are NOT ambiguous. Mirrors the sibling clients.
func createOutcomeAmbiguous(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode >= 500 || ae.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return ae.StatusCode >= 300 && ae.StatusCode < 400 && isMutatingMethod(ae.Method)
}

// truncate returns at most n runes of s, cutting on a rune boundary (never splitting a
// multibyte rune), so an untrusted string embedded in an error can't grow without bound.
// It walks byte offsets rather than materializing a []rune, so a large input isn't
// copied 4× just to keep a short prefix (see the body of the function).
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Walk at most n runes by BYTE index instead of materializing []rune(s): converting
	// a large body (up to maxResponseBytes = 8 MiB) to a rune slice allocates ~4×its
	// size just to keep n runes, which would undermine the response-size guard under
	// concurrency. RuneCountInString is O(len) but allocates nothing.
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	byteEnd := 0
	count := 0
	for i := range s { // ranging a string yields successive rune START byte offsets
		if count == n {
			byteEnd = i
			break
		}
		count++
	}
	// strings.Clone copies just the retained prefix so the result doesn't pin the full
	// (up to 8 MiB) backing array — the exact leak the byte-slice cap here guards against.
	return strings.Clone(s[:byteEnd])
}

// parseNonNegativeInt parses a base-10 non-negative integer, rejecting anything
// with a sign, whitespace, or non-digit characters (so an HTTP-date isn't misread
// as a huge number). Overflow is detected BEFORE each multiply-add: a bare `n < 0`
// check is not sufficient, since n*10+digit can wrap past zero back to a positive
// value (e.g. 18446744073709551617 → 1), which would let an enormous Retry-After be
// read as a tiny wait.
func parseNonNegativeInt(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a non-negative integer: %q", s)
		}
		d := int64(r - '0')
		// Reject before overflowing: if n*10+d would exceed MaxInt64, stop.
		if n > (math.MaxInt64-d)/10 {
			return 0, errors.New("overflow")
		}
		n = n*10 + d
	}
	return n, nil
}
