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
func (c *Client) refreshToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Reuse the cached token while it remains valid past the buffer.
	if c.cachedToken != "" && c.now().Before(c.tokenExpireAt.Add(-redditTokenExpiryBuffer)) {
		return c.cachedToken, nil
	}

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
		return "", fmt.Errorf("reddit token refresh failed: %d: %s", resp.StatusCode, truncate(string(body), 400))
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
		expiresIn = int64(redditTokenExpiryBuffer.Seconds()) + 60
	}

	c.cachedToken = data.AccessToken
	c.tokenExpireAt = c.now().Add(time.Duration(expiresIn) * time.Second)
	return c.cachedToken, nil
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
		return nil, fmt.Errorf("reddit API %s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(raw), 400))
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
	if math.IsNaN(in.BudgetUSD) || math.IsInf(in.BudgetUSD, 0) || in.BudgetUSD <= 0 || in.BudgetUSD > redditMaxBudgetUSD {
		return nil, fmt.Errorf("invalid budget: must be a positive number")
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
		objective = "conversions"
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

	accountID := c.account.AccountID
	label := c.account.Label
	if label == "" {
		label = accountID
	}

	// Step 1: Verify account (non-fatal, mirrors TS try/catch).
	if _, err := c.request(ctx, http.MethodGet, "/ad_accounts/"+accountID, nil); err != nil {
		steps = append(steps, "Account verification warning: "+err.Error())
	} else {
		steps = append(steps, fmt.Sprintf("Account verified: %s (%s)", label, accountID))
	}

	// Normalize geo targets once, up front, so the ad-group label, targeting,
	// and region all derive from a single source of truth.
	geos := make([]string, 0, len(in.GeoTargets))
	for _, g := range in.GeoTargets {
		g = strings.ToUpper(strings.TrimSpace(g))
		if g == "" {
			continue
		}
		geos = append(geos, g)
	}
	if len(geos) == 0 {
		geos = []string{"US"}
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
		"goal_value":                      toMicrodollars(in.BudgetUSD),
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

	usedCommunities := len(communityNames) > 0
	adGroupResp, err := c.request(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ad_groups", buildAdGroupBody(targetingWithCommunities))
	if err != nil {
		if len(communityNames) > 0 && strings.Contains(err.Error(), "invalid communities") {
			steps = append(steps, fmt.Sprintf("Community targeting failed (invalid subreddits: %s), retrying with keywords only", strings.Join(communityNames, ", ")))
			usedCommunities = false
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
	if usedCommunities {
		steps = append(steps, fmt.Sprintf("Targeting: %d communities, %d keywords, %d geos", len(communityNames), len(in.Keywords), len(geos)))
	} else {
		steps = append(steps, fmt.Sprintf("Targeting: %d keywords, %d geos (communities skipped -- add manually in Reddit Ads Manager)", len(in.Keywords), len(geos)))
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
func decodeID(resp *apiResponse) string {
	if resp == nil || len(resp.Data) == 0 {
		return ""
	}
	var obj struct {
		ID any `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &obj); err != nil {
		return ""
	}
	switch v := obj.ID.(type) {
	case string:
		return v
	case float64:
		// Reddit IDs are strings, but guard against numeric widening.
		return strings.TrimSuffix(fmt.Sprintf("%f", v), ".000000")
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
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
		project = "Linux Foundation"
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
	base := strings.TrimRight(in.RegistrationURL, "/")
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

	// Parse and merge UTM params into the URL's query so a URL carrying a
	// fragment (e.g. .../reg#tickets) keeps the fragment at the very end rather
	// than embedding the query inside it.
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
	postPathRe = regexp.MustCompile(`(?i)(?:^|/)(?:r/\w+/)?comments/([a-z0-9]+)`)
	postIDRe   = regexp.MustCompile(`(?i)^[a-z0-9]+$`)
)

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
		if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
			if !isRedditHost(u.Hostname()) {
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
			}
			// redd.it short links: the post ID is the first path segment.
			if strings.EqualFold(u.Hostname(), "redd.it") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".redd.it") {
				id := strings.Trim(u.Path, "/")
				if i := strings.IndexByte(id, '/'); i >= 0 {
					id = id[:i]
				}
				if postIDRe.MatchString(id) {
					return "t3_" + id, nil
				}
				return "", fmt.Errorf("cannot extract Reddit post ID from: %s", trimmed)
			}
			// reddit.com: extract the comments/<id> segment from the path.
			if m := postPathRe.FindStringSubmatch(u.Path); m != nil {
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
