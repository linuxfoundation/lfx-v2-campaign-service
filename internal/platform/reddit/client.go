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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
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
	redditPastStartBuffer = 60 * time.Second
	// redditMaxBudgetUSD caps the budget below the int64 micro-dollar overflow
	// threshold so the ×1e6 conversion in toMicrodollars can't wrap.
	redditMaxBudgetUSD = 1_000_000_000.0
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
	// defaultRedditObjective is used when a campaign input omits an objective.
	defaultRedditObjective = "conversions"
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

// Client is a Reddit Ads API v3 client with cached OAuth token refresh.
// It is safe for concurrent use.
type Client struct {
	creds   Credentials
	account AccountConfig

	baseURL    string
	tokenURL   string
	httpClient *http.Client
	now        func() time.Time

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
		creds:      creds,
		account:    account,
		baseURL:    redditAdsBaseURL,
		tokenURL:   redditTokenURL,
		httpClient: &http.Client{Timeout: redditRequestTimeout},
		now:        time.Now,
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
}

// CampaignResult mirrors RedditCampaignCreateResult.
type CampaignResult struct {
	Platform     string   `json:"platform"`
	CampaignName string   `json:"campaignName"`
	CampaignID   string   `json:"campaignId"`
	AdGroupName  string   `json:"adGroupName"`
	AdGroupID    string   `json:"adGroupId"`
	AdCount      int      `json:"adCount"`
	AdID         string   `json:"adId,omitempty"`
	RedditURL    string   `json:"redditUrl"`
	Steps        []string `json:"steps"`
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
	"video_views": {redditObjective: "VIDEO_VIEWABLE_IMPRESSIONS", bidType: "CPM", optimizationGoal: "VIDEO_VIEWS"},
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
		go func() {
			fetchCtx, cancel := context.WithTimeout(context.Background(), redditRequestTimeout)
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
		if readErr != nil {
			return "", fmt.Errorf("reddit token refresh failed: %d: %s (body read error: %v)", resp.StatusCode, truncate(string(body), redditErrBodyMaxRunes), readErr)
		}
		return "", fmt.Errorf("reddit token refresh failed: %d: %s", resp.StatusCode, truncate(string(body), redditErrBodyMaxRunes))
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

// request performs an authenticated Reddit Ads API call, sanitizing the path
// segments and honoring ctx. Mirrors redditRequest.
func (c *Client) request(ctx context.Context, method, path string, body any) (*apiResponse, error) {
	sanitized := sanitizePath(path)
	fullURL := c.baseURL + sanitized

	token, err := c.refreshToken(ctx)
	if err != nil {
		return nil, err
	}

	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("reddit API %s %s: encode body: %w", method, path, err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("reddit API %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", redditUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reddit API %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := readResponseBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return nil, fmt.Errorf("reddit API %s %s -> %d: %s (body read error: %v)", method, path, resp.StatusCode, truncate(string(raw), redditErrBodyMaxRunes), readErr)
		}
		return nil, fmt.Errorf("reddit API %s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(raw), redditErrBodyMaxRunes))
	}
	if readErr != nil {
		return nil, fmt.Errorf("reddit API %s %s: %w", method, path, readErr)
	}

	var out apiResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("reddit API %s %s: decode response: %w", method, path, err)
		}
	}
	return &out, nil
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
// Campaign creation
// ---------------------------------------------------------------------------

// CreateCampaign creates a PAUSED Reddit campaign with a lifetime budget, an ad
// group with targeting, and (optionally) a promoted-post ad. It mirrors
// executeRedditCampaignCreation. Every step is recorded in the result's Steps.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	var steps []string

	// Validation.
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

	// Step 1: Verify account (non-fatal, mirrors TS try/catch).
	if _, err := c.request(ctx, http.MethodGet, "/ad_accounts/"+accountID, nil); err != nil {
		steps = append(steps, "Account verification warning: "+err.Error())
	} else {
		steps = append(steps, fmt.Sprintf("Account verified: %s (%s)", label, accountID))
	}

	// Compute the effective start time ONCE, before the campaign POST. When the
	// start date is today its midnight-UTC timestamp is already in the past, so
	// nudge it to now+buffer; otherwise use start-of-day. This adjusted value is
	// used for both the campaign and the ad group so nothing sends a past start.
	campaignEndTime := toISOTimestamp(in.EndDate)
	effectiveStart := toISOTimestamp(in.StartDate)
	if startMs, ok := parseRedditTimestamp(effectiveStart); ok && startMs.Before(c.now()) {
		effectiveStart = toRedditTimestamp(c.now().Add(redditPastStartBuffer))
	}
	// After nudging a past start forward, the (unchanged) end could be at/before it
	// for a same-day flight; reject rather than sending an invalid window.
	if sMs, ok1 := parseRedditTimestamp(effectiveStart); ok1 {
		if eMs, ok2 := parseRedditTimestamp(campaignEndTime); ok2 && !eMs.After(sMs) {
			return nil, fmt.Errorf("campaign end %s is not after the effective start %s (a same-day past start was nudged forward)", campaignEndTime, effectiveStart)
		}
	}

	// Step 2: Create campaign (PAUSED, lifetime budget, objective-aware params).
	// objective / objParams were validated above, before the network call.
	campaignName := buildRedditCampaignName(in, objective, resolveRegion(geos))

	campaignData := map[string]any{
		"name":                            campaignName,
		"objective":                       objParams.redditObjective,
		"configured_status":               "PAUSED",
		"is_campaign_budget_optimization": true,
		"bid_strategy":                    "BIDLESS",
		"bid_type":                        objParams.bidType,
		"optimization_goal":               objParams.optimizationGoal,
		"goal_type":                       "LIFETIME_SPEND",
		"goal_value":                      budgetMicros,
		"start_time":                      effectiveStart,
		"end_time":                        campaignEndTime,
	}
	if objParams.viewThroughConversionType != "" {
		campaignData["view_through_conversion_type"] = objParams.viewThroughConversionType
	}

	campaignResp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/campaigns", map[string]any{"data": campaignData})
	if err != nil {
		return nil, err
	}
	campaignID := decodeID(campaignResp)
	if campaignID == "" {
		return nil, fmt.Errorf("reddit campaign creation succeeded but returned no campaign ID")
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, $%.2f lifetime)", campaignID, in.BudgetUSD))

	// Step 3: Create ad group with targeting. The label is built from the same
	// normalized (trimmed, uppercased) geos used in targeting, so a
	// whitespace-padded input can't produce a name inconsistent with targeting.
	geoLabel := strings.Join(geos, "+")
	adGroupName := fmt.Sprintf("Events | %s | %s | Intent | Communities + Keywords", replacePipes(in.EventName), geoLabel)

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

	communityNames := make([]string, 0, len(in.Subreddits))
	for _, s := range in.Subreddits {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(s, "r/"))
		if name == "" {
			continue
		}
		communityNames = append(communityNames, name)
	}

	targetingWithCommunities := baseTargeting
	if len(communityNames) > 0 {
		targetingWithCommunities = cloneTargeting(baseTargeting)
		targetingWithCommunities["communities"] = communityNames
	}

	buildAdGroupBody := func(targeting map[string]any) map[string]any {
		return map[string]any{
			"data": map[string]any{
				"name":              adGroupName,
				"campaign_id":       campaignID,
				"configured_status": "PAUSED",
				"bid_strategy":      "BIDLESS",
				"bid_type":          objParams.bidType,
				"optimization_goal": objParams.optimizationGoal,
				"targeting":         targeting,
				"start_time":        effectiveStart,
				"end_time":          campaignEndTime,
			},
		}
	}

	// suppliedCommunities records whether the caller actually supplied usable
	// subreddits. Only in that case is a fallback (dropping communities) worth
	// warning about; a keyword/geo-only campaign never intended communities.
	suppliedCommunities := len(communityNames) > 0
	usedCommunities := suppliedCommunities
	droppedCommunities := false
	adGroupResp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ad_groups", buildAdGroupBody(targetingWithCommunities))
	if err != nil {
		if suppliedCommunities && strings.Contains(err.Error(), "invalid communities") {
			steps = append(steps, fmt.Sprintf("Community targeting failed (invalid subreddits: %s), retrying without communities", strings.Join(communityNames, ", ")))
			usedCommunities = false
			droppedCommunities = true
			adGroupResp, err = c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ad_groups", buildAdGroupBody(baseTargeting))
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	adGroupID := decodeID(adGroupResp)
	if adGroupID == "" {
		return nil, fmt.Errorf("reddit ad group creation succeeded but returned no ad group ID")
	}
	steps = append(steps, fmt.Sprintf("Ad group created: %s (PAUSED, geo: %s)", adGroupID, strings.Join(geos, ", ")))
	switch {
	case usedCommunities:
		steps = append(steps, fmt.Sprintf("Targeting: %d communities, %d keywords, %d geos", len(communityNames), len(in.Keywords), len(geos)))
	case droppedCommunities:
		// Communities were supplied but the lookup failed, so they were dropped
		// and must be re-added manually.
		steps = append(steps, fmt.Sprintf("Targeting: %d keywords, %d geos (communities skipped -- add manually in Reddit Ads Manager)", len(in.Keywords), len(geos)))
	default:
		// No subreddits were supplied; this is a normal keyword/geo-only campaign.
		steps = append(steps, fmt.Sprintf("Targeting: %d keywords, %d geos", len(in.Keywords), len(geos)))
	}

	// Step 4: Create ad from post URL if provided, otherwise emit instructions.
	adCount := 0
	var adID string

	if in.PostURL != "" && validatedPostID != "" {
		postID := validatedPostID
		steps = append(steps, fmt.Sprintf("Extracted post ID: %s from %s", postID, in.PostURL))

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
			// Distinguish a CALLER context cancellation/deadline from an ordinary
			// per-request failure. If the caller's ctx is done, the whole
			// operation was cancelled: honor the context-aware contract and
			// return a fatal error wrapping ctx.Err() so callers can tell
			// cancellation from success. Key this off the caller ctx, NOT
			// errors.Is on err -- the client's own http.Client.Timeout also
			// surfaces as DeadlineExceeded but with a live caller ctx, and that
			// stays a non-fatal warning like any other per-request failure.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("ad creation aborted: %w", ctxErr)
			}
			steps = append(steps, fmt.Sprintf("Ad creation failed: %s -- add ad manually in Reddit Ads Manager", err.Error()))
		} else {
			adID = decodeID(adResp)
			if adID != "" {
				adCount = 1
				steps = append(steps, fmt.Sprintf("Ad created: %s (post: %s, click URL: %s)", adID, postID, utmURL))
			} else {
				// A 2xx response missing data.id is a malformed success: don't
				// silently count it as a created ad. Surface it as a manual-action
				// warning so the caller knows the ad was not confirmed.
				steps = append(steps, fmt.Sprintf("Ad creation returned no ad ID (malformed response, post: %s) -- add ad manually in Reddit Ads Manager", postID))
			}
		}
	} else {
		variantCount := len(in.Variants)
		if variantCount > 0 {
			steps = append(steps, fmt.Sprintf("%d ad variant(s) ready -- create ads in Reddit Ads Manager with these headlines:", variantCount))
			for i := 0; i < variantCount; i++ {
				utmURL := buildRedditUTMURL(in, i)
				steps = append(steps, fmt.Sprintf("  Variant %d: %q -> %s", i+1, in.Variants[i].Headline, utmURL))
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
	project := in.Project
	if project == "" {
		// The Project segment must be the canonical LFX slug that the data
		// pipeline joins on for attribution; the Linux Foundation's slug is
		// "tlf" (not a display name). See docs/api-catalog.md.
		project = "tlf"
	}
	project = replacePipes(project)
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
		return fmt.Errorf("registration URL %q is not a valid URL: %w", raw, err)
	}
	if !u.IsAbs() || u.Hostname() == "" {
		return fmt.Errorf("registration URL %q must be absolute (include scheme and host)", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", raw, u.Scheme)
	}
}

// buildRedditUTMURL mirrors buildRedditUtmUrl.
func buildRedditUTMURL(in CampaignInput, variantIndex int) string {
	// TrimSpace mirrors validateRegistrationURL, which accepts padded input.
	base := strings.TrimSpace(in.RegistrationURL)
	slug := in.EventSlug
	if slug == "" {
		slug = strings.Join(strings.Fields(strings.ToLower(in.EventName)), "-")
	}
	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}
	term := strings.ToLower(strings.Join(strings.Fields(in.EventName), "-"))

	utm := map[string]string{
		"utm_source":   "reddit",
		"utm_medium":   "paid-social",
		"utm_campaign": campaign,
		"utm_term":     term,
		"utm_content":  fmt.Sprintf("variant-%d", variantIndex+1),
	}

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
		return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
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
			if !isRedditHost(u.Hostname()) {
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
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
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
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
			return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
		}
		return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
	}

	// Otherwise treat the whole input as a bare base36 post ID.
	if postIDRe.MatchString(trimmed) {
		return "t3_" + trimmed, nil
	}
	return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
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

// truncate returns at most n runes of s (never splitting a multi-byte rune).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
