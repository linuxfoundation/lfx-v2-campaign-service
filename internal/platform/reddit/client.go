// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package reddit provides a Go client for the Reddit Ads API v3.
//
// It ports the upstream TypeScript reddit-ads.service.ts client, focusing on
// OAuth 2.0 token refresh (with an expiry buffer) and campaign creation via the
// Campaign -> Ad Group -> Promoted Post (Ad) hierarchy.
//
// Credentials and account configuration are injected via NewClient; the client
// never reads the process environment.
package reddit

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
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
// Reddit Ads API constants (mirrors reddit-ads.service.ts / reddit.constants.ts)
// ---------------------------------------------------------------------------

const (
	// redditAdsBaseURL is the Reddit Ads API v3 base URL.
	redditAdsBaseURL = "https://ads-api.reddit.com/api/v3"
	// redditTokenURL is the OAuth 2.0 access-token endpoint.
	redditTokenURL = "https://www.reddit.com/api/v1/access_token"
	// redditUserAgent is sent on every request per Reddit API requirements.
	redditUserAgent = "LFXAdsManager/1.0"
	// redditAdsManagerURL is the Reddit Ads Manager web URL surfaced in results.
	redditAdsManagerURL = "https://ads.reddit.com"

	// redditRequestTimeout mirrors REDDIT_REQUEST_TIMEOUT_MS (30s).
	redditRequestTimeout = 30 * time.Second
	// redditTokenExpiryBuffer mirrors REDDIT_TOKEN_EXPIRY_BUFFER_SECONDS (60s):
	// refresh the token this long before its stated expiry.
	redditTokenExpiryBuffer = 60 * time.Second
	// redditPastStartBuffer is how far ahead of "now" a start time is nudged when
	// the requested start date is already in the past (e.g. a same-day start,
	// whose midnight-UTC timestamp has passed).
	//
	// The buffer must keep the timestamp in the future across the WHOLE retryable
	// campaign->ad-group workflow, not just at the instant it is computed: request
	// can honor a Retry-After (up to maxRetryWait) and resend the same encoded body
	// on a 429, and this one timestamp is reused for both the campaign POST and the
	// (possibly retried) ad-group POST. Each mutating call can spend up to
	// retryMax*maxRetryWait waiting on 429 backoffs plus retryMax+1 request
	// timeouts; the ad-group step may run twice (the community fallback). Size the
	// buffer to cover that worst case with margin so a nudged start can't have
	// slipped into the past by the time either request is finally accepted.
	// redditWorstCaseCreateWait is the worst-case wall-clock a single mutating
	// create can consume before it is accepted: every 429 backoff (retryMax waits
	// clamped to maxRetryWait), every attempt's HTTP round-trip (retryMax+1
	// request timeouts), AND a token refresh per attempt — refreshToken runs at
	// the start of each attempt and, when the cached token is within the expiry
	// buffer, performs its own bounded HTTP fetch (one request timeout). Counting
	// (retryMax+1) token fetches keeps the buffer conservative so a nudged start
	// can't slip into the past even if every attempt re-fetches the token.
	redditWorstCaseCreateWait = retryMax*maxRetryWait + (retryMax+1)*redditRequestTimeout + (retryMax+1)*redditRequestTimeout
	// redditStartWorkflowBuffer covers the full campaign + up-to-two ad-group
	// creates (the community fallback re-POSTs the ad group) plus a 60s margin, so
	// a nudged start time stays in the future for every request that reuses it.
	redditStartWorkflowBuffer = 3*redditWorstCaseCreateWait + 60*time.Second
	// redditPastStartBuffer is how far ahead of "now" a start time is nudged when
	// the requested start date is already in the past (e.g. a same-day start,
	// whose midnight-UTC timestamp has passed). It reserves enough headroom to
	// cover the full retryable campaign->ad-group workflow (see above) so the
	// nudged start can't be past by the time a retried request is accepted.
	redditPastStartBuffer = redditStartWorkflowBuffer
	// redditMaxBudgetUSD caps the budget below the int64 micro-dollar overflow
	// threshold so the ×1e6 conversion in toMicrodollars can't wrap.
	redditMaxBudgetUSD = 1_000_000_000.0
	// maxEventNameRunes / maxProjectRunes bound the caller-supplied name segments
	// so the composed campaign/ad-group names stay within Reddit's ~200-char name
	// limit (the fixed template segments consume the rest). Validated up front so
	// an over-long name fails before the campaign is created, not after (orphan).
	maxEventNameRunes = 120
	maxProjectRunes   = 40
	// redditMaxNameRunes is Reddit's limit for a campaign/ad-group name. The
	// per-field caps above keep caller inputs bounded, but the COMPOSED name (with
	// region/objective segments) is validated against this before any POST.
	redditMaxNameRunes = 200
	// maxResponseBody bounds how much of any response body is read into memory,
	// guarding against a hostile/oversized reply while comfortably exceeding any
	// normal success or error envelope.
	maxResponseBody = 10 << 20 // 10 MiB
	// redditErrBodyMaxRunes caps how much of an upstream error body is echoed
	// into a returned error string.
	redditErrBodyMaxRunes = 400
	// redditFallbackTokenTTL is the token lifetime assumed when the token
	// endpoint returns a non-positive expires_in: the refresh buffer plus a
	// small margin so a valid-but-lifetimeless token still works without caching
	// an already-expired entry.
	redditFallbackTokenTTL = redditTokenExpiryBuffer + 60*time.Second
	// maxTokenTTLSeconds caps a server-declared expires_in before it is converted
	// from seconds to a time.Duration (int64 nanoseconds). A huge positive
	// expires_in would overflow `time.Duration(expiresIn)*time.Second` and wrap
	// negative, yielding a past expiry that forces a refresh on every call. 24h
	// comfortably exceeds any real Reddit token lifetime while staying far below
	// the ~9.2e9-second overflow point.
	maxTokenTTLSeconds = int64(24 * 60 * 60)
	// defaultRedditObjective is used when a campaign input omits an objective.
	defaultRedditObjective = "conversions"

	// retryMax is the number of times an HTTP 429 (rate-limited) request is
	// retried before giving up. Mirrors the Meta/Twitter clients.
	retryMax = 3
	// retryBaseDelay is the base for exponential backoff when the API returns a
	// 429 without a usable Retry-After header (1s, 2s, 4s, ...). Mirrors Meta.
	retryBaseDelay = 1 * time.Second
	// maxRetryWait caps how long a single 429 backoff waits, so an outsized
	// Retry-After / reset value can't stall a request past the point of
	// usefulness. Mirrors Meta's cap.
	maxRetryWait = 60 * time.Second
)

// readResponseBody reads up to maxResponseBody bytes (plus one, so truncation is
// detectable) from an HTTP response, surfacing both read and truncation errors
// rather than silently discarding partial bytes. io.ReadAll can return bytes
// together with an error, so a discarded error can hide a partial/corrupt body.
func readResponseBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBody+1))
	if err != nil {
		return body, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxResponseBody {
		return body[:maxResponseBody], fmt.Errorf("response body exceeds %d bytes", maxResponseBody)
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// Injected configuration
// ---------------------------------------------------------------------------

// Credentials holds the injected OAuth 2.0 client credentials and refresh
// token used to obtain access tokens. There is no environment lookup anywhere.
type Credentials struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// AccountConfig identifies the Reddit ad account (e.g. "t2_gv9wtbfa") that
// campaigns are created under, plus an optional human-readable label.
type AccountConfig struct {
	AccountID string
	Label     string
}

// Option customizes a Client at construction time.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client (e.g. for tests).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithBaseURL overrides the Reddit Ads API base URL (e.g. for httptest servers).
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithTokenURL overrides the OAuth token endpoint (e.g. for httptest servers).
func WithTokenURL(tokenURL string) Option {
	return func(c *Client) {
		if tokenURL != "" {
			c.tokenURL = tokenURL
		}
	}
}

// WithNowFunc injects a clock source, making token-expiry logic deterministic
// in tests. Defaults to time.Now.
func WithNowFunc(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// withRetryBaseDelay overrides the exponential-backoff base for 429 retries.
// Unexported: only tests use it, to keep retry runs fast (no real multi-second
// sleeps). Mirrors the Meta client's withRetryBaseDelay.
func withRetryBaseDelay(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.retryBaseDelay = d
		}
	}
}

// Client is a Reddit Ads API v3 client with cached OAuth token refresh.
// It is safe for concurrent use.
type Client struct {
	creds   Credentials
	account AccountConfig

	baseURL    string
	tokenURL   string
	httpClient *http.Client
	now        func() time.Time

	// retryBaseDelay is the base for exponential 429 backoff. Defaults to the
	// retryBaseDelay const; tests may shrink it (via withRetryBaseDelay) to keep
	// retry runs fast.
	retryBaseDelay time.Duration

	mu            sync.Mutex
	cachedToken   string
	tokenExpireAt time.Time

	// inflight coalesces concurrent token refreshes with the standard library
	// only (no third-party single-flight). The first caller to find the cache
	// empty/expired becomes the leader: it publishes a *tokenRefresh on inflight
	// and kicks off the fetch in a detached goroutine. Every caller of the same
	// in-flight window — leader and followers alike — selects on its own ctx
	// against the shared tokenRefresh.done channel, then reads the SHARED result
	// (token AND err). This keeps a cold start or expiry burst to a single
	// upstream refresh whose exact outcome (success or failure) is shared by all
	// current waiters, so a failed refresh fails all of them rather than each
	// follower re-leading a fresh refresh in series. Guarded by mu.
	inflight *tokenRefresh
}

// tokenRefresh holds the shared result of one in-flight token refresh. done is
// closed exactly once when the fetch completes, after token/err are set under
// mu; waiters read token/err only after done is closed.
type tokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

// NewClient builds a Client from injected credentials and account config.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		account:        account,
		baseURL:        redditAdsBaseURL,
		tokenURL:       redditTokenURL,
		httpClient:     &http.Client{Timeout: redditRequestTimeout},
		now:            time.Now,
		retryBaseDelay: retryBaseDelay,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// Public campaign-creation types (mirror RedditCampaignCreateRequest/Result)
// ---------------------------------------------------------------------------

// AdVariant mirrors RedditAdVariant.
type AdVariant struct {
	Headline string
	Body     string
}

// CampaignInput mirrors RedditCampaignCreateRequest.
type CampaignInput struct {
	EventName       string
	EventSlug       string
	RegistrationURL string
	HSToken         string
	BudgetUSD       float64
	StartDate       string // YYYY-MM-DD
	EndDate         string // YYYY-MM-DD
	GeoTargets      []string
	Subreddits      []string
	Interests       []string
	Keywords        []string
	Variants        []AdVariant
	Project         string
	Objective       string // one of: awareness, traffic, conversions, video_views
	PostURL         string
	// ConversionPixelID is the Reddit conversion pixel this campaign optimizes
	// toward. It is REQUIRED when the resolved objective is "conversions" (Reddit
	// rejects a conversion ad group without a pixel), and ignored otherwise.
	ConversionPixelID string
	// VideoGoal is the concrete video optimization goal, REQUIRED when the resolved
	// objective is "video_views". Accepted values: VIDEO_VIEW_6S, VIDEO_VIEW_15S.
	// Reddit has no bare "VIDEO_VIEWS" optimization goal, so a concrete goal must
	// be supplied. Ignored for non-video objectives.
	VideoGoal string
}

// CampaignResult mirrors RedditCampaignCreateResult.
type CampaignResult struct {
	Platform     string `json:"platform"`
	CampaignName string `json:"campaignName"`
	CampaignID   string `json:"campaignId"`
	AdGroupName  string `json:"adGroupName"`
	AdGroupID    string `json:"adGroupId"`
	AdCount      int    `json:"adCount"`
	AdID         string `json:"adId,omitempty"`
	// AdWarning is set (non-empty) when a promoted-post ad was attempted but not
	// confirmed — the ad POST failed, or returned a 2xx with no ad id. Ad creation
	// is intentionally non-fatal (the campaign + ad group already succeeded), so
	// the overall error stays nil; this field lets a caller detect the degraded
	// outcome structurally instead of parsing Steps or inferring it from
	// AdCount == 0 (which also covers the valid no-PostURL path). Mirrors the
	// twitter client's promoted-tweet warning.
	AdWarning string   `json:"adWarning,omitempty"`
	RedditURL string   `json:"redditUrl"`
	Steps     []string `json:"steps"`
}

// ---------------------------------------------------------------------------
// Objective parameters (mirrors REDDIT_OBJECTIVE_PARAMS / _LABELS)
// ---------------------------------------------------------------------------

type objectiveParams struct {
	redditObjective           string
	bidType                   string
	optimizationGoal          string
	viewThroughConversionType string
}

// Note: campaigns and ad groups use the BIDLESS bid strategy, so no explicit
// bid value is sent to Reddit. The upstream TS constants carry a bidValue field
// but the reddit-ads.service.ts client never writes it into any request body, so
// it is intentionally omitted here.
var redditObjectiveParams = map[string]objectiveParams{
	"awareness": {redditObjective: "IMPRESSIONS", bidType: "CPM", optimizationGoal: "IMPRESSIONS"},
	"traffic":   {redditObjective: "CLICKS", bidType: "CPC", optimizationGoal: "CLICKS"},
	"conversions": {
		redditObjective:           "CONVERSIONS",
		bidType:                   "CPM",
		optimizationGoal:          "PURCHASE",
		viewThroughConversionType: "SEVEN_DAY_CLICKS_ONE_DAY_VIEW",
	},
	// video_views carries no optimizationGoal here: "VIDEO_VIEWS" is not a valid
	// Reddit video optimization goal, so the concrete goal is resolved from the
	// input's VideoGoal (one of validVideoGoals) and written into the request
	// below rather than baked into this table.
	"video_views": {redditObjective: "VIDEO_VIEWABLE_IMPRESSIONS", bidType: "CPM"},
}

// validVideoGoals is the set of concrete Reddit video optimization goals accepted
// for the "video_views" objective. Reddit has no bare "VIDEO_VIEWS" goal, so a
// video-view campaign must name a concrete goal.
var validVideoGoals = map[string]bool{
	"VIDEO_VIEW_6S":  true,
	"VIDEO_VIEW_15S": true,
}

var redditObjectiveLabels = map[string]string{
	"awareness":   "Awareness",
	"traffic":     "Traffic",
	"conversions": "Conversions",
	"video_views": "Video Views",
}

// geoToRegion maps a primary geo (ISO country code) to a marketing region.
var geoToRegion = map[string]string{
	"US": "NA", "CA": "NA", "MX": "NA",
	"GB": "EMEA", "DE": "EMEA", "FR": "EMEA", "NL": "EMEA", "SE": "EMEA",
	"CH": "EMEA", "ES": "EMEA", "IT": "EMEA", "AT": "EMEA", "BE": "EMEA", "IL": "EMEA",
	"IN": "India",
	"JP": "APAC", "KR": "APAC", "SG": "APAC", "AU": "APAC", "CN": "APAC", "TW": "APAC", "HK": "APAC",
	"BR": "LATAM",
}

// ---------------------------------------------------------------------------
// Auth: token refresh with expiry buffer
// ---------------------------------------------------------------------------

// tokenResponse is the OAuth access-token endpoint payload.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// refreshToken returns a cached access token when it is still valid past the
// expiry buffer, otherwise it requests a new one. Mirrors refreshRedditToken.
//
// Concurrent refreshes are coalesced with a shared-result single-flight so a
// cold start or an expiry burst fires exactly one upstream token request whose
// exact outcome (token AND error) is shared by all current waiters, rather than
// one refresh per in-flight campaign call (which would amplify rate-limit
// pressure). A failed refresh therefore fails all of its current waiters at
// once instead of each of them re-leading a fresh refresh in series. Crucially,
// the lock is NOT held across the network call: the fast path reads the cache
// under a brief lock, and every waiter (leader included) selects on ctx.Done()
// so it can still return promptly with its own context error instead of blocking
// on the shared request indefinitely.
func (c *Client) refreshToken(ctx context.Context) (string, error) {
	// Bail out early if the caller's context is already done, so a cancelled
	// caller never triggers or joins a refresh.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c.mu.Lock()
	// Fast path: reuse the cached token while it remains valid past the buffer.
	if c.cachedToken != "" && c.now().Before(c.tokenExpireAt.Add(-redditTokenExpiryBuffer)) {
		token := c.cachedToken
		c.mu.Unlock()
		return token, nil
	}

	inflight := c.inflight
	if inflight == nil {
		// Become the leader: publish the shared result and kick off the fetch on a
		// bounded context detached from this caller's ctx, so one caller's
		// cancellation can't tear down a refresh other waiters depend on. No lock
		// is held across the network call.
		inflight = &tokenRefresh{done: make(chan struct{})}
		c.inflight = inflight
		// Detach from the caller's CANCELLATION (one caller's cancel must not tear
		// down a refresh other waiters depend on) but PRESERVE its request-scoped
		// VALUES via context.WithoutCancel, so an injected tracing/observability
		// transport can still correlate the token request with the campaign
		// operation. A fresh bounded timeout replaces the caller's deadline.
		refreshValuesCtx := context.WithoutCancel(ctx)
		go func() {
			fetchCtx, cancel := context.WithTimeout(refreshValuesCtx, redditRequestTimeout)
			token, err := c.fetchToken(fetchCtx)
			cancel()

			c.mu.Lock()
			inflight.token = token
			inflight.err = err
			c.inflight = nil
			close(inflight.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()

	// Leader and followers alike wait on the shared result, selecting on their own
	// ctx so a cancelled caller returns promptly with its context error while the
	// detached fetch still completes and populates the shared result and cache for
	// the others. On failure, every current waiter gets the same error and none
	// re-leads a serial refresh.
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-inflight.done:
		return inflight.token, inflight.err
	}
}

// fetchToken performs the actual upstream token request and caches the result.
// It is only ever invoked from the leader's detached refresh goroutine, so at
// most one call is in flight at a time.
func (c *Client) fetchToken(ctx context.Context) (string, error) {
	credentials := base64.StdEncoding.EncodeToString([]byte(c.creds.ClientID + ":" + c.creds.ClientSecret))
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.creds.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("reddit token refresh: build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+credentials)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", redditUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reddit token refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := readResponseBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do NOT echo the OAuth response body: this request carried the client
		// id/secret and refresh token, and a token/proxy diagnostic body is
		// untrusted and may reflect credential material. Since CreateCampaign can
		// persist this error into Steps, expose only the status (and a body-read
		// failure), never the body itself.
		if readErr != nil {
			return "", fmt.Errorf("reddit token refresh failed: HTTP %d (body read error: %v)", resp.StatusCode, readErr)
		}
		return "", fmt.Errorf("reddit token refresh failed: HTTP %d", resp.StatusCode)
	}
	if readErr != nil {
		return "", fmt.Errorf("reddit token refresh: %w", readErr)
	}

	var data tokenResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("reddit token refresh: decode response: %w", err)
	}

	// Reject an empty/malformed token rather than caching garbage.
	if data.AccessToken == "" {
		return "", fmt.Errorf("reddit token refresh: response contained an empty access token")
	}

	// Guard expires_in: a non-positive lifetime is malformed. Fall back to a
	// short default so a valid-but-lifetimeless token still works without
	// caching an already-expired entry.
	expiresIn := data.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = int64(redditFallbackTokenTTL.Seconds())
	}
	// Clamp an outsized expires_in before the seconds->Duration conversion:
	// time.Duration is int64 NANOSECONDS, so expires_in beyond ~9.2e9 seconds
	// would overflow time.Duration(expiresIn)*time.Second and wrap NEGATIVE,
	// producing a past expiry that forces a token refresh on every call. Cap it
	// to maxTokenTTLSeconds so `now + expiresIn*time.Second` can never overflow.
	if expiresIn > maxTokenTTLSeconds {
		expiresIn = maxTokenTTLSeconds
	}

	// Re-acquire the lock only to store the freshly obtained token. Touch
	// cachedToken/tokenExpireAt exclusively under the lock to keep them
	// thread-safe.
	c.mu.Lock()
	c.cachedToken = data.AccessToken
	c.tokenExpireAt = c.now().Add(time.Duration(expiresIn) * time.Second)
	c.mu.Unlock()
	return data.AccessToken, nil
}

// ---------------------------------------------------------------------------
// HTTP helper
// ---------------------------------------------------------------------------

// apiResponse is the common Reddit Ads API envelope: {"data": ...}.
type apiResponse struct {
	Data json.RawMessage `json:"data"`
}

// apiError is returned by request() for a non-2xx Reddit Ads API response. It
// carries the HTTP status so callers can branch on it precisely (e.g. only retry
// an ad-group create WITHOUT communities on a 400 validation error, not on a
// 401/500 where the mutation may have committed and a retry could duplicate it)
// rather than string-matching the response body.
type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	// Deliberately DO NOT include e.Body: the upstream response body is untrusted
	// and can reflect request material (e.g. the click_url's secret query, or a
	// bearer token in a proxy diagnostic). Body is retained on the struct for
	// internal classification (e.g. the "invalid communities" 400 match) but is
	// never surfaced when the error is stringified into Steps / returned to a
	// caller / logged. Report only the method, path, and status.
	return fmt.Sprintf("reddit API %s %s -> %d", e.Method, e.Path, e.StatusCode)
}

// transportError wraps a failure of the HTTP round-trip itself (httpClient.Do):
// the request was ALREADY SENT, so the server may or may not have processed it —
// the outcome is AMBIGUOUS. This is distinct from a pre-send failure (token
// refresh, body encode, request build) or a definite abort, where the request
// never reached the server and a mutation definitely did not happen. Callers use
// it to decide whether a failed create is "may exist" (ambiguous) vs "not
// created".
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("reddit API %s %s: %v", e.Method, e.Path, e.Err)
}
func (e *transportError) Unwrap() error { return e.Err }

// createOutcomeAmbiguous reports whether a failed mutating request MAY have been
// applied by Reddit despite the error — i.e. the request plausibly reached the
// server and its outcome is unknowable. It is the single source of truth shared
// by the campaign, ad-group, and ad create paths so they classify identically:
//   - transportError: the round-trip failed AFTER a connection was established
//     (see isPreSendDialError — a DNS/dial/connection-refused/TLS-handshake
//     failure is NOT wrapped as transportError, so it never reaches here), so the
//     request may have been received. This is ALSO the path a context
//     cancellation/deadline from the in-flight Do takes: the per-attempt timeout
//     wraps the whole round trip, so a ctx error can fire after the POST reached
//     Reddit, and request() wraps it as transportError so it is treated as
//     ambiguous (UNCONFIRMED), never definitely-failed;
//   - apiError with a 5xx status: Reddit received it and may have committed the
//     mutation before erroring.
//
// A definite 4xx (Reddit rejected it), or any pre-send failure (token refresh,
// body encode/build, a pre-connect dial/TLS-handshake error, or a caller-cancel
// that surfaces raw BEFORE the POST — e.g. from refreshToken), means NOT applied
// → returns false so the caller returns a clean (nil, err) / "failed" rather than
// "may exist".
func createOutcomeAmbiguous(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *apiError
	return errors.As(err, &ae) && ae.StatusCode >= 500
}

// isPreSendDialError reports whether a httpClient.Do error clearly happened
// BEFORE any request bytes could have reached the server, so the request was NOT
// sent and must NOT be treated as an ambiguous "may exist" transportError. It
// covers ONLY failures that PROVE no request body was transmitted:
//   - DNS resolution failure (the host never resolved);
//   - connection refused / no route / network unreachable (never connected);
//   - TLS handshake / certificate errors (the secure channel was never
//     established, so no request body was sent).
//
// A context cancellation/deadline is deliberately NOT treated as pre-send here:
// the per-attempt attemptCtx wraps the ENTIRE round trip (send + response read),
// so a context.Canceled/DeadlineExceeded surfacing from Do can fire AFTER the
// POST body already reached Reddit (Reddit may have created the resource; we
// just never read the response). Classifying that as pre-send would let a caller
// treat a possibly-created campaign as definitely-failed and retry, risking a
// double-create. Such ctx errors therefore fall through to the transportError
// wrapping in request(), which createOutcomeAmbiguous treats as ambiguous
// (UNCONFIRMED) — never FAILED. A genuine caller-cancel before any POST is still
// handled precisely by the ctx.Err() checks in the create path.
//
// A failure AFTER a connection is established and bytes were sent (mid-flight
// timeout, unexpected EOF on the response) is genuinely ambiguous and IS wrapped
// as transportError.
func isPreSendDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	// TLS handshake / certificate verification failures: the secure channel was
	// never established, so no request bytes were sent.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var recordErr tls.RecordHeaderError
	return errors.As(err, &recordErr)
}

// request performs an authenticated Reddit Ads API call, sanitizing the path
// segments and honoring ctx. Mirrors redditRequest.
//
// An HTTP 429 (rate-limited) response is retried up to retryMax times with a
// bounded backoff, mirroring the Meta/Twitter clients: CreateCampaign issues
// several sequential Reddit API calls that can trip Reddit's per-account rate
// limits mid-flow. The wait honors Reddit's Retry-After header when present
// (delay-seconds or HTTP-date), else falls back to exponential
// retryBaseDelay*2^attempt, in both cases clamped to maxRetryWait. If a
// server-declared reset exceeds maxRetryWait, the request aborts with the
// rate-limit error rather than sleeping past the point of usefulness. A 429 is
// retried regardless of HTTP method: a 429 means the request was REJECTED
// before processing (nothing was created), so retrying a create POST is safe.
func (c *Client) request(ctx context.Context, method, path string, body any) (*apiResponse, error) {
	// Split any query string off before sanitizing: sanitizePath escapes each
	// PATH segment (turning a literal '?'/'=' into %3F/%3D), which would corrupt a
	// query. The query (built by the caller via url.Values.Encode) is already
	// percent-encoded, so it is re-appended verbatim after the sanitized path.
	rawPath, rawQuery, hasQuery := strings.Cut(path, "?")
	sanitized := sanitizePath(rawPath)
	if hasQuery {
		sanitized += "?" + rawQuery
	}
	fullURL := c.baseURL + sanitized

	// Marshal the body once; a fresh reader is created per attempt below since
	// bytes.NewReader is consumed by the first send.
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("reddit API %s %s: encode body: %w", method, path, err)
		}
	}

	for attempt := 0; attempt <= retryMax; attempt++ {
		// Refresh (cached/coalesced) each attempt so a token that expired between
		// retries is renewed. The reader is rebuilt every attempt because the
		// previous send consumed it.
		token, err := c.refreshToken(ctx)
		if err != nil {
			return nil, err
		}

		var reqBody io.Reader
		if encoded != nil {
			reqBody = bytes.NewReader(encoded)
		}

		// Bound EACH attempt with a per-attempt context deadline, not just the
		// http.Client.Timeout: a WithHTTPClient override could supply a client with
		// no Timeout, and request() takes the caller ctx directly (which may be
		// context.Background()), so without this a create could hang indefinitely —
		// and redditWorstCaseCreateWait (which sizes the past-start buffer) assumes
		// every attempt is capped by redditRequestTimeout.
		attemptCtx, cancel := context.WithTimeout(ctx, redditRequestTimeout)
		req, err := http.NewRequestWithContext(attemptCtx, method, fullURL, reqBody)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("reddit API %s %s: build request: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", redditUserAgent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			cancel()
			// A Do error that clearly happened BEFORE the request could be sent (DNS
			// failure, connection refused, no route) means NOT sent — return it plain
			// so callers treat the create as "not applied". A failure after a
			// connection was established (mid-flight timeout, EOF) is genuinely
			// ambiguous: wrap it as transportError so callers treat the create as
			// "may exist".
			if isPreSendDialError(err) {
				return nil, fmt.Errorf("reddit API %s %s: %w", method, path, err)
			}
			return nil, &transportError{Method: method, Path: path, Err: err}
		}

		// A 429 with retries remaining: compute the wait and back off. The body is
		// drained (to EOF, up to maxResponseBody) and then closed before sleeping
		// so Go's transport can reuse the underlying TCP/TLS connection for the
		// retry -- closing without draining forces a fresh connection per retry,
		// which is exactly the wrong behavior while rate-limited. The LimitReader
		// keeps a hostile/oversized body from being read unbounded. On the final
		// attempt we fall through and surface the 429 as an ordinary non-2xx error
		// below rather than looping forever.
		if resp.StatusCode == http.StatusTooManyRequests && attempt < retryMax {
			retryAfter, ok := c.parseRetryAfter(resp)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
			_ = resp.Body.Close()
			cancel() // release the per-attempt deadline before backing off / retrying
			if ok {
				// The server declared a reset time. If it exceeds our cap, sleeping
				// would burn a retry without any chance of the window clearing, so
				// abort with the rate-limit error (mirrors Meta/Twitter). Report the
				// RAW Retry-After header, not the parsed duration: parseRetryAfter
				// clamps an over-cap value to a maxRetryWait+1s sentinel, so printing
				// the duration would misreport (e.g. a 3600s reset as "1m1s").
				if retryAfter > maxRetryWait {
					return nil, fmt.Errorf("reddit API %s %s -> 429: rate-limit reset (Retry-After: %s) exceeds max wait %s; aborting", method, path, resp.Header.Get("Retry-After"), maxRetryWait)
				}
				if err := sleepCtx(ctx, retryAfter); err != nil {
					return nil, err
				}
				continue
			}
			// No usable header: exponential backoff clamped to the cap.
			wait := c.retryBaseDelay * time.Duration(1<<uint(attempt))
			if wait > maxRetryWait {
				wait = maxRetryWait
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		raw, readErr := readResponseBody(resp.Body)
		_ = resp.Body.Close()
		cancel() // body fully read; release the per-attempt deadline
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body := truncate(string(raw), redditErrBodyMaxRunes)
			if readErr != nil {
				body = fmt.Sprintf("%s (body read error: %v)", body, readErr)
			}
			return nil, &apiError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: body}
		}
		// A 2xx whose body can't be read or decoded means the mutation SUCCEEDED but
		// we can't parse the result — wrap as transportError so callers classify it
		// as ambiguous ("may exist"), not a clean not-created failure. (A caller
		// retry off a plain error could duplicate the resource.)
		if readErr != nil {
			return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("read 2xx response body: %w", readErr)}
		}

		var out apiResponse
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, &transportError{Method: method, Path: path, Err: fmt.Errorf("decode 2xx response: %w", err)}
			}
		}
		return &out, nil
	}

	// Unreachable in practice: the loop returns the 429 as a non-2xx error on the
	// final attempt. Kept as a defensive backstop mirroring Meta/Twitter.
	return nil, fmt.Errorf("reddit API %s %s -> exhausted %d retries after 429s", method, path, retryMax)
}

// parseRetryAfter returns how long to wait before retrying a 429 and whether a
// usable header was found. Reddit sends Retry-After either as a delay in seconds
// or as an HTTP-date; both forms are honored (mirrors Meta's parseRetryAfter).
// A non-positive/overflowing delay, a past HTTP-date, or an absent header all
// return (0, false), signaling the caller to fall back to exponential backoff.
// The returned duration is never negative.
func (c *Client) parseRetryAfter(resp *http.Response) (time.Duration, bool) {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	// Delay-seconds form. ParseInt (not Atoi) so a non-numeric/overflowing
	// STRING is treated as unusable rather than silently wrapping.
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n > 0 {
			// Even a validly-parsed int64 seconds value can overflow when scaled
			// to nanoseconds: time.Duration(n)*time.Second wraps NEGATIVE for n
			// beyond ~9.2e9, which would slip past the caller's `> maxRetryWait`
			// abort and trigger an immediate retry. Guard the conversion: any n
			// STRICTLY ABOVE the max-wait ceiling (in seconds) already exceeds the
			// cap, so report a duration just over maxRetryWait (usable=true) and
			// let the caller's over-cap abort fire -- never perform the wrapping
			// multiply. A value EXACTLY at the cap (e.g. Retry-After: 60 with a 60s
			// cap) is allowed and returned as-is, so it isn't spuriously aborted.
			if n > int64(maxRetryWait/time.Second) {
				return maxRetryWait + time.Second, true
			}
			return time.Duration(n) * time.Second, true
		}
		return 0, false
	}
	// HTTP-date form: the duration until that instant, relative to the injected
	// clock. A date already in the past is unusable.
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// sleepCtx waits for d, returning early with ctx.Err() if ctx is cancelled.
// Mirrors the Meta/Twitter clients' ctx-honoring sleep.
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

// sanitizePath re-encodes each path segment, mirroring the TS segment sanitizer.
func sanitizePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		decoded, err := url.PathUnescape(s)
		if err != nil {
			decoded = s
		}
		segments[i] = url.PathEscape(decoded)
	}
	return strings.Join(segments, "/")
}

// ---------------------------------------------------------------------------
// Subreddit name -> ID resolution
// ---------------------------------------------------------------------------

// stripSubredditPrefix removes a leading "r/" prefix case-insensitively (so
// "R/golang", "r/golang", and "golang" all yield "golang"). The remainder is
// returned unchanged; the caller trims surrounding whitespace.
func stripSubredditPrefix(s string) string {
	if len(s) >= 2 && (s[0] == 'r' || s[0] == 'R') && s[1] == '/' {
		return s[2:]
	}
	return s
}

// ---------------------------------------------------------------------------
// Campaign creation
// ---------------------------------------------------------------------------

// CreateCampaign creates a PAUSED Reddit campaign with a lifetime budget, an ad
// group with targeting, and (optionally) a promoted-post ad. It mirrors
// executeRedditCampaignCreation. Every step is recorded in the result's Steps.
//
// PARTIAL-RESULT CONTRACT: once the campaign POST succeeds, a subsequent
// AD-GROUP failure (or a caller-context cancellation mid-flow) returns BOTH a
// non-nil *CampaignResult (carrying the created, PAUSED campaign id and the
// steps completed so far) AND a non-nil error. This is deliberate so the
// orphaned paid resource is identifiable for cleanup/reconciliation. Callers
// MUST NOT follow the usual `if err != nil { return err }` pattern that discards
// the result: inspect the returned *CampaignResult (e.g. CampaignID) even when
// err != nil, or a retry may create a duplicate campaign. Before the campaign
// POST succeeds, a failure returns (nil, err) as usual.
//
// AD creation (the optional promoted post) is by contrast NON-fatal: the
// campaign and ad group already succeeded, so an ad-POST failure or an
// unconfirmed (no-id) ad returns a nil error with the degraded outcome recorded
// BOTH in Steps and, structurally, in CampaignResult.AdWarning (so a caller can
// detect it without parsing Steps, and can tell it apart from the valid
// no-PostURL case where AdCount is also 0). A caller-context cancellation during
// the ad POST is the exception — it is fatal and follows the partial-result rule
// above.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	var steps []string

	// Validation.
	// EventName is required by the campaign-create contract and feeds the campaign
	// / ad-group / ad names + attribution. Reject an empty/whitespace value up
	// front (before any mutating call) so it can't create paid resources with an
	// empty name segment. Mirrors the meta/twitter clients.
	if strings.TrimSpace(in.EventName) == "" {
		return nil, fmt.Errorf("event name is required")
	}
	// Project must be the caller's canonical LFX slug — it is the Project segment
	// of the campaign name that the data pipeline joins on for foundation
	// attribution. Per the api-catalog contract this service must stamp it from
	// the authenticated project rather than trust free text, and it must NOT
	// silently default (a hardcoded default mis-attributes every non-TLF campaign
	// to the Linux Foundation). Reject an empty value before any network call,
	// mirroring the twitter/meta clients. Trim once so downstream name/attribution
	// consumers all see the same value.
	in.Project = strings.TrimSpace(in.Project)
	if in.Project == "" {
		return nil, fmt.Errorf("project is required: supply the canonical LFX project slug for the campaign name's Project segment")
	}
	// Normalize EventName in place too so the campaign name, ad-group/ad names,
	// and UTM fields all use the same trimmed value (a padded name otherwise
	// leaks into attribution, e.g. utm_term=-kubecon-).
	in.EventName = strings.TrimSpace(in.EventName)
	// Length-validate EventName and Project up front (by rune count): both are
	// folded into the campaign and ad-group names, and an over-long value would be
	// rejected by Reddit only at the ad-group POST — AFTER the campaign already
	// exists, orphaning it. Fail before any mutating call instead. The bounds leave
	// headroom for the fixed name-template segments within Reddit's name limit.
	if n := utf8.RuneCountInString(in.EventName); n > maxEventNameRunes {
		return nil, fmt.Errorf("event name is too long: %d characters (max %d)", n, maxEventNameRunes)
	}
	if n := utf8.RuneCountInString(in.Project); n > maxProjectRunes {
		return nil, fmt.Errorf("project is too long: %d characters (max %d)", n, maxProjectRunes)
	}
	switch {
	case math.IsNaN(in.BudgetUSD) || math.IsInf(in.BudgetUSD, 0) || in.BudgetUSD <= 0:
		return nil, fmt.Errorf("invalid budget: must be a positive number")
	case in.BudgetUSD > redditMaxBudgetUSD:
		return nil, fmt.Errorf("invalid budget: must be a finite value in (0, %.0f]", redditMaxBudgetUSD)
	}
	// A positive-but-tiny budget can still round to zero micro-dollars (e.g.
	// 0.0000001 USD), which would send goal_value: 0 to Reddit. Budgets are
	// rounded to the nearest micro-dollar (round-half-up), so the effective
	// floor is a value that rounds to at least one micro-dollar (>= 0.0000005
	// USD); reject anything below that.
	budgetMicros := toMicrodollars(in.BudgetUSD)
	if budgetMicros <= 0 {
		return nil, fmt.Errorf("invalid budget: %g USD rounds to zero micro-dollars; must round to at least one micro-dollar (>= 0.0000005 USD)", in.BudgetUSD)
	}
	// Parse for calendar validity (rejects e.g. 2026-02-31), not just format.
	startDate, err := time.Parse("2006-01-02", in.StartDate)
	if err != nil {
		return nil, fmt.Errorf("invalid start date: %s -- expected a valid YYYY-MM-DD", in.StartDate)
	}
	endDate, err := time.Parse("2006-01-02", in.EndDate)
	if err != nil {
		return nil, fmt.Errorf("invalid end date: %s -- expected a valid YYYY-MM-DD", in.EndDate)
	}
	if !endDate.After(startDate) {
		return nil, fmt.Errorf("end date %s must be after start date %s", in.EndDate, in.StartDate)
	}

	// Validate the registration URL before any mutating call: it becomes the ad
	// destination, so an empty/malformed value would otherwise be sent to Reddit
	// (or embedded in a UTM URL) after paid resources already exist.
	if err := validateRegistrationURL(in.RegistrationURL); err != nil {
		return nil, err
	}

	// Validate the objective before any network round-trip, so an unsupported
	// objective fails fast rather than after the Step 1 account-verify call.
	objective := in.Objective
	if objective == "" {
		objective = defaultRedditObjective
	}
	objParams, objOK := redditObjectiveParams[objective]
	if !objOK {
		return nil, fmt.Errorf("unsupported Reddit objective: %s", objective)
	}

	// Resolve the effective optimization goal. Most objectives carry a static goal
	// in objParams; "video_views" resolves it from VideoGoal because Reddit has no
	// bare "VIDEO_VIEWS" goal. Validate before any mutating call so a bad/empty
	// goal fails fast rather than after the campaign POST orphans a PAUSED campaign.
	optimizationGoal := objParams.optimizationGoal
	if objective == "video_views" {
		videoGoal := strings.ToUpper(strings.TrimSpace(in.VideoGoal))
		if !validVideoGoals[videoGoal] {
			return nil, fmt.Errorf("invalid video goal %q for objective video_views: must be one of VIDEO_VIEW_6S, VIDEO_VIEW_15S", in.VideoGoal)
		}
		optimizationGoal = videoGoal
	}

	// Validate/resolve the conversion pixel before any mutating call. Reddit
	// requires a conversion pixel for a conversion ad group, so a missing pixel
	// would create the campaign in Step 2 and then fail at ad-group creation,
	// orphaning a PAUSED campaign. Reject up front for the "conversions" objective
	// so nothing is created.
	//
	// conversionPixelID is left EMPTY for every non-conversion objective even if the
	// caller supplied a value: the field is documented as ignored outside
	// conversions, and the payloads below add conversion_pixel_id only when this is
	// non-empty. Gating resolution on the objective (not merely on the input being
	// non-empty) keeps a reused input carrying a stray pixel from sending an
	// objective-inapplicable field that Reddit would reject.
	var conversionPixelID string
	if objective == "conversions" {
		conversionPixelID = strings.TrimSpace(in.ConversionPixelID)
		if conversionPixelID == "" {
			return nil, fmt.Errorf("conversion pixel ID is required for objective conversions")
		}
	}

	var validatedPostID string
	if in.PostURL != "" {
		id, err := extractRedditPostID(in.PostURL)
		if err != nil {
			return nil, err
		}
		validatedPostID = id
	}

	// Validate the account ID before any request path is built. It is
	// concatenated into request paths ("/ad_accounts/<id>/...") before
	// sanitizePath splits on "/", so an ID containing a slash would inject extra
	// path segments and ID values like "." or ".." could be reinterpreted by an
	// upstream server or proxy. A non-empty check is not enough; enforce a safe
	// charset up front. Reddit account IDs look like "t2_xxxxx" (alphanumeric +
	// underscore), so restrict to that charset.
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return nil, fmt.Errorf("reddit account ID is required")
	}
	if !accountIDRe.MatchString(accountID) {
		return nil, fmt.Errorf("invalid reddit account ID %q: must contain only letters, digits, and underscores", accountID)
	}
	label := c.account.Label
	if label == "" {
		label = accountID
	}

	// Normalize and validate geo targets BEFORE any mutating call. Reddit expects
	// ISO 3166-1 alpha-2 codes; a bad value like "USA" or "US/CA" would otherwise
	// pass local checks, create the campaign in Step 2, then fail at ad-group
	// creation — orphaning the campaign. Reject up front, naming the bad value, so
	// nothing is created. Empty/whitespace entries are skipped (not errors).
	geos := make([]string, 0, len(in.GeoTargets))
	for _, g := range in.GeoTargets {
		g = strings.ToUpper(strings.TrimSpace(g))
		if g == "" {
			continue
		}
		if !geoCodeRE.MatchString(g) || !iso3166Alpha2[g] {
			return nil, fmt.Errorf("invalid geo target %q: must be an ISO 3166-1 alpha-2 country code", g)
		}
		geos = append(geos, g)
	}
	if len(geos) == 0 {
		geos = []string{"US"}
	}

	// Step 1: Verify account (non-fatal, mirrors TS try/catch). A verification
	// FAILURE is only a warning — but a CALLER context cancellation is fatal:
	// continuing would run the campaign POST under a dead ctx and then
	// mis-report an "unconfirmed, may exist" partial even though nothing was
	// created. Abort here on ctx cancellation, before any mutating call.
	if _, err := c.request(ctx, http.MethodGet, "/ad_accounts/"+accountID, nil); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("reddit campaign creation aborted during account verification: %w", ctxErr)
		}
		steps = append(steps, "Account verification warning: "+err.Error())
	} else {
		steps = append(steps, fmt.Sprintf("Account verified: %s (%s)", label, accountID))
	}

	// Extract the supplied subreddit names (strip an optional "r/" prefix, drop
	// blanks and case-insensitive duplicates). Reddit ad-group `communities`
	// targeting takes subreddit NAMES, not t5_ IDs — sending IDs is rejected as
	// "invalid communities". This matches the reference TS implementation
	// (reddit-ads.service.ts sends the r/-stripped names directly). If Reddit
	// rejects any name the POST below falls back to keyword/geo-only targeting
	// with a warning step, so a bad name never orphans a PAUSED campaign.
	communityNames := make([]string, 0, len(in.Subreddits))
	seenCommunity := make(map[string]struct{}, len(in.Subreddits))
	for _, s := range in.Subreddits {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		name := strings.TrimSpace(stripSubredditPrefix(s))
		if name == "" {
			continue
		}
		// De-duplicate case-insensitively (subreddit names are case-insensitive);
		// preserve the first-seen casing for the payload/warnings.
		key := strings.ToLower(name)
		if _, dup := seenCommunity[key]; dup {
			continue
		}
		seenCommunity[key] = struct{}{}
		communityNames = append(communityNames, name)
	}

	// Compute the effective start time ONCE, before the campaign POST. When the
	// Nudge the start forward whenever it does not already clear the workflow
	// HORIZON (now + redditPastStartBuffer), not merely when it is already past: a
	// start only a few seconds/minutes ahead could slip into the past DURING
	// account verification, the token exchange, or a 429 retry, since this one
	// timestamp is reused for the campaign and the (possibly retried) ad-group
	// POST. redditPastStartBuffer is sized (see its definition) to cover that whole
	// retryable campaign->ad-group workflow, so a start at/after now+buffer is
	// guaranteed to still be future when every request that reuses it is accepted;
	// anything earlier is nudged up to now+buffer.
	campaignEndTime := toISOTimestamp(in.EndDate)
	effectiveStart := toISOTimestamp(in.StartDate)
	horizon := c.now().Add(redditPastStartBuffer)
	if startMs, ok := parseRedditTimestamp(effectiveStart); !ok || startMs.Before(horizon) {
		effectiveStart = toRedditTimestamp(horizon)
	}
	// After nudging a past start forward, the (unchanged) end could be at/before
	// it; reject rather than sending an invalid window.
	if sMs, ok1 := parseRedditTimestamp(effectiveStart); ok1 {
		if eMs, ok2 := parseRedditTimestamp(campaignEndTime); ok2 && !eMs.After(sMs) {
			return nil, fmt.Errorf("campaign end %s is not after the effective start %s (a past start date was nudged forward)", campaignEndTime, effectiveStart)
		}
	}

	// Compute BOTH composed names (campaign + ad group) and validate their lengths
	// up front — before the campaign POST. The per-field EventName/Project bounds
	// keep caller inputs sane, but the composed names also include region/objective
	// (campaign) and a "+"-joined geo list (ad group), and GeoTargets has no count
	// limit, so enough valid codes could push adGroupName past Reddit's limit.
	// Validating both here means an over-limit name fails BEFORE any paid resource
	// exists, rather than orphaning the campaign at the ad-group step.
	campaignName := buildRedditCampaignName(in, objective, resolveRegion(geos))
	geoLabel := strings.Join(geos, "+")
	adGroupName := fmt.Sprintf("Events | %s | %s | Intent | Communities + Keywords", replacePipes(in.EventName), geoLabel)
	if n := utf8.RuneCountInString(campaignName); n > redditMaxNameRunes {
		return nil, fmt.Errorf("composed campaign name is too long: %d characters (max %d)", n, redditMaxNameRunes)
	}
	if n := utf8.RuneCountInString(adGroupName); n > redditMaxNameRunes {
		return nil, fmt.Errorf("composed ad group name is too long: %d characters (max %d)", n, redditMaxNameRunes)
	}

	campaignData := map[string]any{
		"name":                            campaignName,
		"objective":                       objParams.redditObjective,
		"configured_status":               "PAUSED",
		"is_campaign_budget_optimization": true,
		"bid_strategy":                    "BIDLESS",
		"bid_type":                        objParams.bidType,
		"optimization_goal":               optimizationGoal,
		"goal_type":                       "LIFETIME_SPEND",
		"goal_value":                      budgetMicros,
		"start_time":                      effectiveStart,
		"end_time":                        campaignEndTime,
	}
	if objParams.viewThroughConversionType != "" {
		campaignData["view_through_conversion_type"] = objParams.viewThroughConversionType
	}
	// A conversion campaign carries the conversion pixel it optimizes toward.
	if conversionPixelID != "" {
		campaignData["conversion_pixel_id"] = conversionPixelID
	}

	campaignResp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/campaigns", map[string]any{"data": campaignData})
	if err != nil {
		// A CALLER context cancellation (even one that surfaced as a transportError
		// during an EARLIER step like account verification, then propagated here) is
		// NOT an ambiguous create: no campaign POST completed. Honor the documented
		// pre-POST (nil, err) contract rather than returning a misleading "may exist"
		// Classify AMBIGUITY FIRST, before the ctx check: a cancellation that
		// interrupts the create's in-flight round-trip surfaces as a transportError,
		// and the campaign MAY already have been committed — so it must be treated as
		// "may exist", NOT a clean pre-POST abort. createOutcomeAmbiguous is true only
		// when the request plausibly reached Reddit (transportError or a 5xx).
		if !createOutcomeAmbiguous(err) {
			// Not ambiguous → the campaign was definitely NOT created: a pre-send
			// failure (token refresh, body encode/build, a pre-connect dial error), a
			// 429 over-cap abort, a definite 4xx, or a cancellation before the request
			// went out. Return (nil, err) so a caller can retry safely. If a caller
			// cancellation is the cause, surface it as an abort.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("reddit campaign creation aborted before completion: %w", ctxErr)
			}
			return nil, err
		}
		steps = append(steps, "Campaign creation is UNCONFIRMED (the create request reached Reddit but the outcome is unknown); a PAUSED campaign may exist -- verify by name in Reddit Ads Manager before retrying to avoid a duplicate")
		return &CampaignResult{
			Platform:     "reddit-ads",
			CampaignName: campaignName,
			RedditURL:    redditAdsManagerURL,
			Steps:        steps,
		}, fmt.Errorf("reddit campaign creation UNCONFIRMED (a PAUSED campaign %q may exist): %w", campaignName, err)
	}
	campaignID := decodeID(campaignResp)
	if campaignID == "" {
		// A 2xx with no data.id is a malformed success: Reddit may have created a
		// PAUSED campaign whose id we couldn't read. Return a partial result carrying
		// the campaign NAME (and steps) alongside the error, so a caller can find and
		// reconcile the orphan by name instead of discarding everything (mirrors the
		// ad-group/ad malformed-success handling). CampaignID stays empty.
		steps = append(steps, "Campaign creation returned no campaign ID (malformed response); a PAUSED campaign may exist -- verify by name in Reddit Ads Manager")
		return &CampaignResult{
			Platform:     "reddit-ads",
			CampaignName: campaignName,
			RedditURL:    redditAdsManagerURL,
			Steps:        steps,
		}, fmt.Errorf("reddit campaign creation succeeded but returned no campaign ID (a PAUSED campaign %q may exist)", campaignName)
	}
	// Use %g (not %.2f) so a sub-cent accepted budget isn't misreported as $0.00 —
	// the step reflects the value actually sent, preserving its precision.
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, $%g lifetime)", campaignID, in.BudgetUSD))

	// campaignName / adGroupName were composed and length-validated up front (before
	// the campaign POST). The ad-group name is deterministic so partialResult can
	// include it: on an ad-group failure or a 2xx-without-id malformed success, an
	// ad group may exist and its name is the only reconciliation handle.

	// partialResult builds a *CampaignResult carrying the already-created (PAUSED)
	// campaign id, the deterministic ad-group name, and the steps completed so far.
	// It is returned ALONGSIDE the error whenever an ad-group step fails after the
	// campaign POST already succeeded, so the orphaned PAUSED campaign (and a
	// possibly-created ad group, identifiable by name) is reconcilable for cleanup
	// and a caller retry doesn't blindly create a duplicate. This only makes the
	// orphan IDENTIFIABLE -- it does not resume creation. True retry-safe
	// idempotency needs provider idempotency keys / the orchestrator claim, tracked
	// in LFXV2-2665.
	partialResult := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "reddit-ads",
			CampaignName: campaignName,
			CampaignID:   campaignID,
			AdGroupName:  adGroupName,
			RedditURL:    redditAdsManagerURL,
			Steps:        steps,
		}
	}

	// Step 3: Create ad group with targeting.

	baseTargeting := map[string]any{
		"geolocations":     geos,
		"locations":        []string{"FEED", "COMMENTS_PAGE"},
		"platforms":        []string{"ALL"},
		"expand_targeting": true,
	}
	if len(in.Keywords) > 0 {
		baseTargeting["keywords"] = in.Keywords
	}
	if len(in.Interests) > 0 {
		baseTargeting["interests"] = in.Interests
	}

	targetingWithCommunities := baseTargeting
	if len(communityNames) > 0 {
		targetingWithCommunities = cloneTargeting(baseTargeting)
		targetingWithCommunities["communities"] = communityNames
	}

	buildAdGroupBody := func(targeting map[string]any) map[string]any {
		data := map[string]any{
			"name":              adGroupName,
			"campaign_id":       campaignID,
			"configured_status": "PAUSED",
			"bid_strategy":      "BIDLESS",
			"bid_type":          objParams.bidType,
			"optimization_goal": optimizationGoal,
			"targeting":         targeting,
			"start_time":        effectiveStart,
			"end_time":          campaignEndTime,
		}
		// Reddit requires the conversion pixel on a conversion ad group; without it
		// the ad-group create fails and orphans the PAUSED campaign.
		if conversionPixelID != "" {
			data["conversion_pixel_id"] = conversionPixelID
		}
		return map[string]any{"data": data}
	}

	// suppliedCommunities records whether the caller supplied usable subreddits.
	// Only then is a 400 "invalid communities" fallback (dropping communities)
	// worth warning about; a keyword/geo-only campaign never intended communities.
	suppliedCommunities := len(communityNames) > 0
	usedCommunities := suppliedCommunities
	droppedCommunities := false
	adGroupResp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ad_groups", buildAdGroupBody(targetingWithCommunities))
	// adGroupErr words an ad-group failure as UNCONFIRMED when the outcome is
	// ambiguous (transportError / 5xx — the ad group MAY exist, and partialResult
	// carries its deterministic name for reconciliation) vs a flat "failed" for a
	// definite not-created error (4xx / pre-send). A caller-cancel that interrupted
	// the in-flight POST surfaces as a transportError, so it too is UNCONFIRMED.
	adGroupErr := func(e error) error {
		if createOutcomeAmbiguous(e) {
			return fmt.Errorf("reddit ad group creation UNCONFIRMED (campaign %s created, PAUSED; the ad group %q may exist — verify before recreating): %w", campaignID, adGroupName, e)
		}
		return fmt.Errorf("reddit ad group creation failed (campaign %s created, PAUSED): %w", campaignID, e)
	}
	if err != nil {
		// Only fall back to a communities-less retry on a 400 (validation) response
		// whose body names invalid communities. Restricting to 400 (and matching the
		// phrase case-insensitively) avoids retrying after a 401/500 — an ambiguous
		// failure where the ad group may already have been created, so a blind retry
		// could duplicate it.
		var apiErr *apiError
		is400InvalidCommunities := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
			strings.Contains(strings.ToLower(apiErr.Body), "invalid communities")
		if suppliedCommunities && is400InvalidCommunities {
			steps = append(steps, fmt.Sprintf("Community targeting failed (invalid subreddits: %s), retrying without communities", strings.Join(communityNames, ", ")))
			usedCommunities = false
			droppedCommunities = true
			adGroupResp, err = c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ad_groups", buildAdGroupBody(baseTargeting))
			if err != nil {
				return partialResult(), adGroupErr(err)
			}
		} else {
			return partialResult(), adGroupErr(err)
		}
	}

	adGroupID := decodeID(adGroupResp)
	if adGroupID == "" {
		// A 2xx with no ad-group id is a malformed success — the ad group may exist;
		// treat it as UNCONFIRMED (partialResult carries the name for reconcile).
		return partialResult(), fmt.Errorf("reddit ad group creation UNCONFIRMED: 2xx returned no ad group ID (campaign %s created, PAUSED; ad group %q may exist)", campaignID, adGroupName)
	}
	steps = append(steps, fmt.Sprintf("Ad group created: %s (PAUSED, geo: %s)", adGroupID, strings.Join(geos, ", ")))
	switch {
	case usedCommunities:
		steps = append(steps, fmt.Sprintf("Targeting: %d communities, %d keywords, %d geos", len(communityNames), len(in.Keywords), len(geos)))
	case droppedCommunities:
		// Communities were supplied but the upstream rejected them ("invalid
		// communities"), so they were dropped and must be re-added manually.
		steps = append(steps, fmt.Sprintf("Targeting: %d keywords, %d geos (communities skipped -- add manually in Reddit Ads Manager)", len(in.Keywords), len(geos)))
	default:
		// No subreddits were supplied; this is a normal keyword/geo-only campaign.
		steps = append(steps, fmt.Sprintf("Targeting: %d keywords, %d geos", len(in.Keywords), len(geos)))
	}

	// Step 4: Create ad from post URL if provided, otherwise emit instructions.
	adCount := 0
	var adID string
	var adWarning string

	if in.PostURL != "" && validatedPostID != "" {
		postID := validatedPostID
		steps = append(steps, fmt.Sprintf("Extracted post ID: %s from %s", postID, redactURL(in.PostURL)))

		utmURL := buildRedditUTMURL(in, 0)
		adBody := map[string]any{
			"data": map[string]any{
				"ad_group_id":       adGroupID,
				"name":              replacePipes(in.EventName) + " - Ad",
				"post_id":           postID,
				"configured_status": "PAUSED",
				"click_url":         utmURL,
			},
		}

		adResp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ads", adBody)
		if err != nil {
			// A caller context cancellation is fatal (return an error, not just a
			// warning). Whether the ad "may exist" depends on whether the request was
			// in flight: an ambiguous error (transportError / 5xx — which is also how a
			// cancellation that interrupted the in-flight POST surfaces) means the ad
			// MAY exist; a clean pre-send cancel means it does not. Return a partial
			// result carrying both IDs so the (created, PAUSED) campaign+ad group are
			// reconcilable regardless.
			if ctxErr := ctx.Err(); ctxErr != nil {
				pr := partialResult()
				pr.AdGroupName = adGroupName
				pr.AdGroupID = adGroupID
				if createOutcomeAmbiguous(err) {
					pr.AdWarning = "promoted-post ad is UNCONFIRMED: the create was interrupted in flight and the ad MAY exist — verify in Reddit Ads Manager before recreating"
					return pr, fmt.Errorf("ad creation interrupted, outcome UNCONFIRMED: %w", ctxErr)
				}
				return pr, fmt.Errorf("ad creation aborted before send: %w", ctxErr)
			}
			// Not a caller cancel. Classify by outcome:
			//   - ambiguous (transportError / 5xx): the ad MAY exist — UNCONFIRMED,
			//     require verification before manual creation (avoids a duplicate);
			//   - definite (4xx / pre-send): the ad was NOT created — report FAILED so
			//     the operator's manual remediation isn't blocked by a misleading
			//     "may exist".
			// Report ONLY the HTTP status, never err.Error()/apiError.Body: a
			// reflective Reddit validation error can echo the click_url (which holds
			// the caller's permitted secret-bearing query params); persisting it in
			// Steps would leak those.
			var adAPIErr *apiError
			gotStatus := errors.As(err, &adAPIErr)
			if createOutcomeAmbiguous(err) {
				adWarning = "promoted-post ad creation is UNCONFIRMED (campaign and ad group created, PAUSED); the ad request may have reached Reddit — verify whether the ad exists BEFORE creating it manually to avoid a duplicate"
				if gotStatus {
					steps = append(steps, fmt.Sprintf("Ad creation UNCONFIRMED (HTTP %d) -- verify in Reddit Ads Manager before adding manually", adAPIErr.StatusCode))
				} else {
					steps = append(steps, "Ad creation UNCONFIRMED (request may have reached Reddit) -- verify in Reddit Ads Manager before adding manually")
				}
			} else {
				// This definite-not-created branch covers BOTH a 4xx (Reddit received
				// and REJECTED the ad) and a pre-send failure (token refresh, request
				// build, or a pre-connect dial error — Reddit NEVER received it). Word
				// each accurately rather than claiming "Reddit rejected" in both cases.
				if gotStatus {
					adWarning = "promoted-post ad creation FAILED (campaign and ad group created, PAUSED); Reddit rejected the ad — add it manually in Reddit Ads Manager"
					steps = append(steps, fmt.Sprintf("Ad creation failed: Reddit rejected the ad (HTTP %d) -- add ad manually in Reddit Ads Manager", adAPIErr.StatusCode))
				} else {
					adWarning = "promoted-post ad creation FAILED before it reached Reddit (campaign and ad group created, PAUSED); the ad was not created — add it manually in Reddit Ads Manager"
					steps = append(steps, "Ad creation failed before the request reached Reddit -- add ad manually in Reddit Ads Manager")
				}
			}
		} else {
			adID = decodeID(adResp)
			if adID != "" {
				adCount = 1
				// Display a sanitized click URL (utm_* only, no userinfo / original
				// query) so a secret in RegistrationURL isn't persisted in Steps.
				steps = append(steps, fmt.Sprintf("Ad created: %s (post: %s, click URL: %s)", adID, postID, displayRedditUTMURL(in, 0)))
			} else {
				// A 2xx response missing data.id is a malformed success — Reddit may
				// still have created the ad. Don't count it as created, and don't tell
				// the operator to add it (which could duplicate it); mark it UNCONFIRMED
				// and require verification first, matching the transport-error path.
				adWarning = "promoted-post ad creation is UNCONFIRMED (2xx response with no ad id); the ad may already exist — verify in Reddit Ads Manager BEFORE creating it manually to avoid a duplicate"
				steps = append(steps, fmt.Sprintf("Ad creation UNCONFIRMED (2xx with no ad ID, post: %s) -- verify in Reddit Ads Manager before adding manually", postID))
			}
		}
	} else {
		variantCount := len(in.Variants)
		if variantCount > 0 {
			// The click URLs below show ONLY the generated utm_* parameters on the
			// destination — any pre-existing query on the registration URL is omitted
			// here to avoid persisting a secret in the returned steps. When building the
			// ads manually, use YOUR registration URL (with its own query params intact)
			// as the base and add these utm_* parameters, so the destination matches
			// what an automated ad would use.
			steps = append(steps, fmt.Sprintf("%d ad variant(s) ready -- create ads in Reddit Ads Manager with these headlines (append the shown utm_* params to your registration URL, keeping its existing query):", variantCount))
			for i := 0; i < variantCount; i++ {
				steps = append(steps, fmt.Sprintf("  Variant %d: %q -> %s", i+1, in.Variants[i].Headline, displayRedditUTMURL(in, i)))
			}
		} else {
			steps = append(steps, "No ad variants or post URL provided -- add ads manually in Reddit Ads Manager")
		}
	}

	return &CampaignResult{
		Platform:     "reddit-ads",
		CampaignName: campaignName,
		CampaignID:   campaignID,
		AdGroupName:  adGroupName,
		AdGroupID:    adGroupID,
		AdCount:      adCount,
		AdID:         adID,
		AdWarning:    adWarning,
		RedditURL:    redditAdsManagerURL,
		Steps:        steps,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers (mirror the TS pure functions)
// ---------------------------------------------------------------------------

// decodeID extracts data.id from a Reddit API envelope, returning "" if absent.
// Reddit IDs are strings; a non-string id (bool, number, object, array) is
// treated as absent rather than coerced into a bogus value like "true" or
// "map[]" that a caller might mistake for a valid resource ID. The id is
// trimmed of surrounding whitespace so a blank/whitespace-only id (e.g.
// {"data":{"id":" "}}) is treated as absent rather than as a created resource.
func decodeID(resp *apiResponse) string {
	if resp == nil || len(resp.Data) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(resp.Data, &obj); err != nil {
		return ""
	}
	id, ok := obj["id"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(id)
}

// toMicrodollars mirrors toMicrodollars: USD -> integer micro-dollars.
func toMicrodollars(usd float64) int64 {
	return int64(math.Round(usd * 1_000_000))
}

// toRedditTimestamp mirrors toRedditTimestamp: RFC3339 with a +00:00 offset and
// no fractional seconds, e.g. 2026-07-09T00:00:00+00:00.
func toRedditTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05") + "+00:00"
}

// toISOTimestamp mirrors toIsoTimestamp: YYYY-MM-DD -> Reddit timestamp at
// midnight UTC.
func toISOTimestamp(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		// Validation upstream guarantees a valid date; fall back defensively.
		return dateStr + "T00:00:00+00:00"
	}
	return toRedditTimestamp(t.UTC())
}

// parseRedditTimestamp parses a Reddit +00:00 timestamp back to a time.Time.
func parseRedditTimestamp(ts string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02T15:04:05-07:00", ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// resolveRegion mirrors resolveRegion.
func resolveRegion(geoTargets []string) string {
	if len(geoTargets) == 0 {
		return "Global"
	}
	primary := strings.ToUpper(strings.TrimSpace(geoTargets[0]))
	if region, ok := geoToRegion[primary]; ok {
		return region
	}
	return "Global"
}

// buildRedditCampaignName mirrors buildRedditCampaignName.
func buildRedditCampaignName(in CampaignInput, objective, region string) string {
	event := replacePipes(in.EventName)
	objectiveLabel := redditObjectiveLabels[objective]
	if objectiveLabel == "" {
		objectiveLabel = "Conversions"
	}
	// in.Project is required and already trimmed/validated in CreateCampaign (no
	// silent default — a hardcoded slug would mis-attribute non-TLF campaigns).
	project := replacePipes(in.Project)
	return fmt.Sprintf("Events | %s | %s | %s | Intent | Social | %s | ToFU", event, region, objectiveLabel, project)
}

// validateRegistrationURL ensures a user-supplied registration URL is an
// absolute http/https URL with a real host before it is used as an ad
// destination. url.Parse accepts "https://:443/path" (Host=":443") where
// Hostname() is empty, so check Hostname() rather than just Host.
func validateRegistrationURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("registration URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// Redact before echoing, and do NOT wrap the url.Parse error: a *url.Error
		// embeds the full raw URL in its own message, which would re-expose a secret
		// in userinfo/query even though we redacted the %q argument. Keep only the
		// redacted URL in the returned error.
		return fmt.Errorf("registration URL %q is not a valid URL", redactURL(raw))
	}
	if !u.IsAbs() || u.Hostname() == "" {
		return fmt.Errorf("registration URL %q must be absolute (include scheme and host)", redactURL(raw))
	}
	// url.Parse does NOT validate percent-encoding in the query. A URL like
	// ".../reg?token=%zz" parses fine here, but u.Query() (used by
	// buildRedditUTMURL) silently DROPS the malformed pair, so the paid ad would be
	// created with a different destination than the caller supplied. Reject a
	// malformed query up front, before any mutating call.
	if _, qerr := url.ParseQuery(u.RawQuery); qerr != nil {
		return fmt.Errorf("registration URL %q has a malformed query string", redactURL(raw))
	}
	// Reject embedded userinfo (user[:password]@host): an ad destination never
	// needs URL credentials, and buildRedditUTMURL would otherwise forward the
	// password to Reddit as click_url and echo it in the success step, leaking it.
	if u.User != nil {
		return fmt.Errorf("registration URL must not contain embedded credentials (userinfo)")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", redactURL(raw), u.Scheme)
	}
}

// redditUTMParams returns the exact set of utm_* parameters this client
// generates for a click URL. It is the single source of truth for both the real
// click URL (buildRedditUTMURL) and the sanitized display URL
// (displayRedditUTMURL), so the display allowlist can never drift from what is
// actually generated.
func redditUTMParams(in CampaignInput, variantIndex int) map[string]string {
	slug := in.EventSlug
	if slug == "" {
		slug = strings.Join(strings.Fields(strings.ToLower(in.EventName)), "-")
	}
	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}
	term := strings.ToLower(strings.Join(strings.Fields(in.EventName), "-"))
	return map[string]string{
		"utm_source":   "reddit",
		"utm_medium":   "paid-social",
		"utm_campaign": campaign,
		"utm_term":     term,
		"utm_content":  fmt.Sprintf("variant-%d", variantIndex+1),
	}
}

// buildRedditUTMURL mirrors buildRedditUtmUrl.
func buildRedditUTMURL(in CampaignInput, variantIndex int) string {
	// TrimSpace mirrors validateRegistrationURL, which accepts padded input.
	base := strings.TrimSpace(in.RegistrationURL)
	utm := redditUTMParams(in, variantIndex)

	// Parse FIRST, then trim a trailing slash from the PATH only. Trimming the
	// raw URL string would corrupt a value like .../reg?next=/ (dropping the
	// trailing query value). Parsing first also keeps a fragment (e.g.
	// .../reg#tickets) at the very end rather than embedding the query inside it.
	u, err := url.Parse(base)
	if err != nil {
		// Fall back to naive concatenation if the URL can't be parsed.
		params := url.Values{}
		for k, v := range utm {
			params.Set(k, v)
		}
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		return base + sep + params.Encode()
	}
	// Strip a single genuine trailing "/" separator without corrupting an
	// encoded %2F that is part of a path segment. u.Path is the DECODED path, so
	// trimming it can't distinguish "/reg/" from "/reg%2F" and would also
	// invalidate u.RawPath. Decide using EscapedPath(), which keeps %2F literal:
	// only a literal trailing "/" is a real separator.
	escaped := u.EscapedPath()
	if strings.HasSuffix(escaped, "/") {
		trimmed := strings.TrimSuffix(escaped, "/")
		// Round-trip the trimmed escaped path so both u.Path (decoded) and
		// u.RawPath (encoded) stay consistent and %2F is preserved.
		if decoded, err := url.PathUnescape(trimmed); err == nil {
			u.Path = decoded
			u.RawPath = trimmed
		}
	}
	q := u.Query()
	for k, v := range utm {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// displayRedditUTMURL builds a click URL safe to persist in Steps / return to
// callers: it strips any userinfo and any PRE-EXISTING query parameters from the
// registration URL (which may carry secrets like ?token=...) and keeps ONLY the
// generated utm_* parameters. The full URL — including the caller's original
// query — is still sent to Reddit as the real click_url; only the display copy
// is sanitized. variantIndex mirrors buildRedditUTMURL.
func displayRedditUTMURL(in CampaignInput, variantIndex int) string {
	u, err := url.Parse(strings.TrimSpace(in.RegistrationURL))
	if err != nil {
		// Fall back to a plain redaction (scheme+host+path) if the URL won't parse
		// — never return the raw value with its secrets.
		return redactURL(in.RegistrationURL)
	}
	u.User = nil    // drop any basic-auth userinfo
	u.Fragment = "" // a fragment can carry sensitive data; drop it for display
	// Rebuild the query from ONLY the utm_* params THIS client generates (with our
	// values), discarding the caller's entire original query. Filtering the merged
	// query by a "utm_" prefix would be unsafe: a caller-supplied ?utm_secret=... or
	// ?utm_source=<override> would survive. An explicit allowlist from the shared
	// generator is the source of truth.
	safe := url.Values{}
	for k, v := range redditUTMParams(in, variantIndex) {
		safe.Set(k, v)
	}
	u.RawQuery = safe.Encode()
	return u.String()
}

var (
	// Extract the post ID from the URL PATH only. The host is validated
	// separately (see isRedditHost) so a path segment can never masquerade as
	// the authority (e.g. https://evil.example/.reddit.com/comments/abc123).
	//
	// The pattern is anchored to the START of the parsed path ("^/"), so only a
	// canonical Reddit post route is accepted: "/r/<subreddit>/comments/<id>" or
	// "/comments/<id>". Anchoring matters because "comments/<id>" can otherwise
	// appear anywhere in a path — e.g. "/user/comments/abc123" (a user overview)
	// or "/foo/comments/abc123" — and must NOT be promoted to a post ID; the
	// caller did not supply a real post route.
	//
	// The ID capture is also anchored to a proper path-segment boundary: the ID
	// must be followed by end-of-string, a "/", or the query/fragment delimiters
	// "?"/"#". Without this, a malformed segment like "comments/abc123!!!" would
	// match and be silently truncated to "t3_abc123"; the boundary makes such
	// trailing junk fail to match so it is rejected rather than accepted.
	postPathRe = regexp.MustCompile(`(?i)^/(?:r/\w+/)?comments/([a-z0-9]+)(?:[/?#]|$)`)
	postIDRe   = regexp.MustCompile(`(?i)^[a-z0-9]+$`)
	// accountIDRe restricts a Reddit ad-account ID to a safe charset (letters,
	// digits, underscore) so it cannot inject extra path segments or "."/".."
	// when concatenated into a request path.
	accountIDRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	// geoCodeRE matches the shape of an ISO 3166-1 alpha-2 country code (two
	// ASCII letters). Shape alone is insufficient — a well-shaped but bogus code
	// like "XX" must still be rejected — so callers pair it with iso3166Alpha2.
	geoCodeRE = regexp.MustCompile(`^[A-Z]{2}$`)
)

// iso3166Alpha2 is the set of assigned ISO 3166-1 alpha-2 country codes. Reddit
// expects GeoTargets as alpha-2 codes (docs/api-catalog.md), so we reject
// anything that is not a valid 2-letter code before any mutating call — a bad
// value like "USA" or "US/CA" would otherwise pass local validation, create the
// campaign, then fail at ad-group creation and orphan the campaign.
var iso3166Alpha2 = map[string]bool{
	"AD": true, "AE": true, "AF": true, "AG": true, "AI": true, "AL": true, "AM": true, "AO": true,
	"AQ": true, "AR": true, "AS": true, "AT": true, "AU": true, "AW": true, "AX": true, "AZ": true,
	"BA": true, "BB": true, "BD": true, "BE": true, "BF": true, "BG": true, "BH": true, "BI": true,
	"BJ": true, "BL": true, "BM": true, "BN": true, "BO": true, "BQ": true, "BR": true, "BS": true,
	"BT": true, "BV": true, "BW": true, "BY": true, "BZ": true, "CA": true, "CC": true, "CD": true,
	"CF": true, "CG": true, "CH": true, "CI": true, "CK": true, "CL": true, "CM": true, "CN": true,
	"CO": true, "CR": true, "CU": true, "CV": true, "CW": true, "CX": true, "CY": true, "CZ": true,
	"DE": true, "DJ": true, "DK": true, "DM": true, "DO": true, "DZ": true, "EC": true, "EE": true,
	"EG": true, "EH": true, "ER": true, "ES": true, "ET": true, "FI": true, "FJ": true, "FK": true,
	"FM": true, "FO": true, "FR": true, "GA": true, "GB": true, "GD": true, "GE": true, "GF": true,
	"GG": true, "GH": true, "GI": true, "GL": true, "GM": true, "GN": true, "GP": true, "GQ": true,
	"GR": true, "GS": true, "GT": true, "GU": true, "GW": true, "GY": true, "HK": true, "HM": true,
	"HN": true, "HR": true, "HT": true, "HU": true, "ID": true, "IE": true, "IL": true, "IM": true,
	"IN": true, "IO": true, "IQ": true, "IR": true, "IS": true, "IT": true, "JE": true, "JM": true,
	"JO": true, "JP": true, "KE": true, "KG": true, "KH": true, "KI": true, "KM": true, "KN": true,
	"KP": true, "KR": true, "KW": true, "KY": true, "KZ": true, "LA": true, "LB": true, "LC": true,
	"LI": true, "LK": true, "LR": true, "LS": true, "LT": true, "LU": true, "LV": true, "LY": true,
	"MA": true, "MC": true, "MD": true, "ME": true, "MF": true, "MG": true, "MH": true, "MK": true,
	"ML": true, "MM": true, "MN": true, "MO": true, "MP": true, "MQ": true, "MR": true, "MS": true,
	"MT": true, "MU": true, "MV": true, "MW": true, "MX": true, "MY": true, "MZ": true, "NA": true,
	"NC": true, "NE": true, "NF": true, "NG": true, "NI": true, "NL": true, "NO": true, "NP": true,
	"NR": true, "NU": true, "NZ": true, "OM": true, "PA": true, "PE": true, "PF": true, "PG": true,
	"PH": true, "PK": true, "PL": true, "PM": true, "PN": true, "PR": true, "PS": true, "PT": true,
	"PW": true, "PY": true, "QA": true, "RE": true, "RO": true, "RS": true, "RU": true, "RW": true,
	"SA": true, "SB": true, "SC": true, "SD": true, "SE": true, "SG": true, "SH": true, "SI": true,
	"SJ": true, "SK": true, "SL": true, "SM": true, "SN": true, "SO": true, "SR": true, "SS": true,
	"ST": true, "SV": true, "SX": true, "SY": true, "SZ": true, "TC": true, "TD": true, "TF": true,
	"TG": true, "TH": true, "TJ": true, "TK": true, "TL": true, "TM": true, "TN": true, "TO": true,
	"TR": true, "TT": true, "TV": true, "TW": true, "TZ": true, "UA": true, "UG": true, "UM": true,
	"US": true, "UY": true, "UZ": true, "VA": true, "VC": true, "VE": true, "VG": true, "VI": true,
	"VN": true, "VU": true, "WF": true, "WS": true, "YE": true, "YT": true, "ZA": true, "ZM": true,
	"ZW": true,
}

// isRedditHost reports whether host is exactly reddit.com / redd.it or a
// subdomain of either. Matching on the parsed authority (not a substring of the
// whole URL) prevents SSRF/spoofing via an attacker-controlled host or path.
func isRedditHost(host string) bool {
	host = strings.ToLower(host)
	switch {
	case host == "reddit.com" || strings.HasSuffix(host, ".reddit.com"):
		return true
	case host == "redd.it" || strings.HasSuffix(host, ".redd.it"):
		return true
	default:
		return false
	}
}

// extractRedditPostID mirrors extractRedditPostId.
func extractRedditPostID(urlOrID string) (string, error) {
	trimmed := strings.TrimSpace(urlOrID)

	// A t3_-prefixed raw ID takes precedence over URL parsing.
	if rest, ok := strings.CutPrefix(trimmed, "t3_"); ok {
		// Validate the base36 remainder; reject inputs like "t3_!!!" or "t3_".
		if postIDRe.MatchString(rest) {
			return trimmed, nil
		}
		return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
	}

	// If it looks like a URL, validate the HOST is genuinely Reddit before
	// trusting anything in the path.
	if strings.Contains(trimmed, "/") || strings.Contains(trimmed, ".") {
		// Normalize a scheme-less URL (e.g. "reddit.com/r/go/comments/abc123" or
		// "redd.it/abc123"): url.Parse puts a scheme-less authority in Path with an
		// empty Host, which the host check below would reject. The TS extractor
		// accepts both forms, so prepend https:// when no scheme is present.
		parseTarget := trimmed
		if !strings.Contains(trimmed, "://") {
			parseTarget = "https://" + trimmed
		}
		if u, err := url.Parse(parseTarget); err == nil && u.Host != "" {
			// Reject embedded userinfo (e.g. token@reddit.com/...): Reddit post links
			// never require URL credentials, and the raw PostURL is later echoed into
			// Steps, which would expose the token. Treat it as unparseable.
			if u.User != nil {
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
			}
			if !isRedditHost(u.Hostname()) {
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
			}
			// redd.it short links: the post ID is the first path segment.
			// Match against EscapedPath(), not the decoded u.Path (same reason as
			// the reddit.com branch below): an encoded delimiter like %2F stays
			// literal in EscapedPath but decodes to a real '/' in u.Path, so
			// matching the decoded path would let "redd.it/abc123%2Fjunk" (whose
			// single segment really ends in "%2Fjunk") be split into "abc123" and
			// wrongly accepted as t3_abc123.
			if strings.EqualFold(u.Hostname(), "redd.it") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".redd.it") {
				id := strings.Trim(u.EscapedPath(), "/")
				if i := strings.IndexByte(id, '/'); i >= 0 {
					id = id[:i]
				}
				if postIDRe.MatchString(id) {
					return "t3_" + id, nil
				}
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
			}
			// reddit.com: extract the comments/<id> segment from the path.
			// Match against EscapedPath(), not the decoded u.Path: an encoded
			// delimiter like %3F ('?') or %2F ('/') stays literal in EscapedPath
			// but decodes to a real delimiter in u.Path. Matching the decoded
			// path would let "/comments/abc123%3Fjunk" (whose id segment really
			// ends in "%3Fjunk") look like "/comments/abc123?junk" and be
			// accepted as t3_abc123, smuggling trailing junk into a valid id.
			if m := postPathRe.FindStringSubmatch(u.EscapedPath()); m != nil {
				return "t3_" + m[1], nil
			}
			return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
		}
		return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
	}

	// Otherwise treat the whole input as a bare base36 post ID.
	if postIDRe.MatchString(trimmed) {
		return "t3_" + trimmed, nil
	}
	return "", fmt.Errorf("cannot extract Reddit post ID from: %s", redactURL(trimmed))
}

// replacePipes replaces "|" with "-" so composed names stay unambiguous.
func replacePipes(s string) string {
	return strings.ReplaceAll(s, "|", "-")
}

// cloneTargeting shallow-copies a targeting map so a variant can add a key
// without mutating the base.
func cloneTargeting(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// redactURL returns a URL safe to persist in a result step: scheme://host/path
// only, dropping the query and fragment (which can carry sensitive tokens) and
// any userinfo. If the input does not parse as an absolute URL, the raw string
// is truncated defensively so a malformed value can't bloat the step, and only
// the portion before any '?' or '#' is kept so an unparsed query/fragment still
// isn't echoed.
func redactURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if u, err := url.Parse(trimmed); err == nil && u.IsAbs() && u.Host != "" {
		redacted := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		return redacted.String()
	}
	// Unparseable / non-absolute input CANNOT be redacted reliably — e.g. a value
	// like "https://<user>:<pass>@example.com/%zz" fails url.Parse and its authority
	// (with userinfo) would otherwise be echoed. Rather than risk leaking a secret,
	// strip the query/fragment AND anything up to and including a userinfo "@", and
	// if a credential delimiter is present at all, don't echo the value.
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	// A "user:pass@host" authority still carries the credential; if an "@" remains
	// after dropping the query/fragment, redact the whole value.
	if strings.Contains(trimmed, "@") {
		return "[unparseable-url-redacted]"
	}
	return truncate(trimmed, redditErrBodyMaxRunes)
}

// truncate returns at most n runes of s (never splitting a multi-byte rune).
func truncate(s string, n int) string {
	// Walk runes only up to the cutoff instead of converting the whole string to
	// []rune — the input can be a large upstream error body (up to maxResponseBody),
	// and the full conversion would allocate/scan all of it just to keep the first
	// n runes.
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	// Fewer than (or exactly) n runes: return the whole string unchanged.
	return s
}
