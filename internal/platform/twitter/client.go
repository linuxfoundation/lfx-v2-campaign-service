// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package twitter is a Go port of the X (Twitter) Ads platform client. It
// implements OAuth 1.0a (HMAC-SHA1) request signing and the
// campaign -> line_item -> promoted_tweet creation flow against the X Ads API.
//
// Credentials and account configuration are injected via NewClient; this
// package never reads environment variables or touches the database. In
// production the credentials come from a decrypted stored connection.
package twitter

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // OAuth 1.0a mandates HMAC-SHA1; not used for security hashing.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Constants (mirror twitter.constants.ts + shared constants)
// ---------------------------------------------------------------------------

const (
	// DefaultBaseURL is the X Ads API origin. Mirrors TWITTER_ADS_BASE_URL.
	DefaultBaseURL = "https://ads-api.x.com"
	// DefaultAPIVersion mirrors TWITTER_ADS_API_VERSION.
	DefaultAPIVersion = "12"
	// AdsManagerURL mirrors TWITTER_ADS_MANAGER_URL.
	AdsManagerURL = "https://ads.x.com"

	// requestTimeout mirrors TWITTER_REQUEST_TIMEOUT_MS.
	requestTimeout = 30 * time.Second
	// writeDelay mirrors TWITTER_API_WRITE_DELAY_MS (1 write req/sec).
	writeDelay = 1 * time.Second
	// retryMax mirrors TWITTER_API_RETRY_MAX.
	retryMax = 3
)

// ---------------------------------------------------------------------------
// Injected configuration
// ---------------------------------------------------------------------------

// Credentials holds the OAuth 1.0a user-context credentials required for all
// X Ads API write operations. These are injected, never read from the
// environment.
type Credentials struct {
	ConsumerKey       string
	ConsumerSecret    string
	AccessToken       string
	AccessTokenSecret string
}

// AccountConfig identifies the ads account and its funding instrument.
type AccountConfig struct {
	AccountID           string
	FundingInstrumentID string
}

// Client is an X Ads API client. It is safe for sequential use; the X Ads API
// enforces a 1 write-request-per-second limit which this client honors.
type Client struct {
	creds   Credentials
	account AccountConfig

	baseURL    string
	apiVersion string
	httpClient *http.Client

	// nonceFn and timeFn are injectable for deterministic testing of the
	// OAuth signature. Production code uses the crypto/rand + wall-clock
	// defaults installed by NewClient.
	nonceFn func() string
	timeFn  func() time.Time
}

// Option customizes a Client at construction time.
type Option func(*Client)

// WithBaseURL overrides the API base URL (default DefaultBaseURL).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithAPIVersion overrides the API version segment (default DefaultAPIVersion).
func WithAPIVersion(v string) Option { return func(c *Client) { c.apiVersion = v } }

// WithHTTPClient overrides the underlying *http.Client (default has a 30s timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// NewClient constructs a Client from injected credentials and account config.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	c := &Client{
		creds:      creds,
		account:    account,
		baseURL:    DefaultBaseURL,
		apiVersion: DefaultAPIVersion,
		httpClient: &http.Client{Timeout: requestTimeout},
		nonceFn:    defaultNonce,
		timeFn:     time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// OAuth 1.0a — HMAC-SHA1 signing
// ---------------------------------------------------------------------------

// percentEncode implements RFC 3986 percent-encoding as required by OAuth 1.0a.
// It mirrors the TS percentEncode (encodeURIComponent + escaping !'()*).
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// generateOAuthSignature computes the HMAC-SHA1 base64 signature over the
// OAuth 1.0a signature base string. Mirrors generateOAuthSignature in the TS.
func generateOAuthSignature(method, u string, params map[string]string, consumerSecret, tokenSecret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, percentEncode(k)+"="+percentEncode(params[k]))
	}
	paramString := strings.Join(parts, "&")

	baseString := strings.ToUpper(method) + "&" + percentEncode(u) + "&" + percentEncode(paramString)
	signingKey := percentEncode(consumerSecret) + "&" + percentEncode(tokenSecret)

	mac := hmac.New(sha1.New, []byte(signingKey))
	mac.Write([]byte(baseString))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildOAuthHeader builds the "Authorization: OAuth ..." header for a request.
// bodyParams is retained to mirror the TS signature but is unused in practice
// (JSON bodies are not form params and are not signed by X Ads).
func (c *Client) buildOAuthHeader(method, rawURL string, bodyParams map[string]string) (string, error) {
	oauthParams := map[string]string{
		"oauth_consumer_key":     c.creds.ConsumerKey,
		"oauth_nonce":            c.nonceFn(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(c.timeFn().Unix(), 10),
		"oauth_token":            c.creds.AccessToken,
		"oauth_version":          "1.0",
	}

	// allParams = oauthParams + bodyParams + query params, used only for signing.
	allParams := make(map[string]string, len(oauthParams)+len(bodyParams))
	for k, v := range oauthParams {
		allParams[k] = v
	}
	for k, v := range bodyParams {
		allParams[k] = v
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	for k, vs := range parsed.Query() {
		if len(vs) > 0 {
			allParams[k] = vs[0]
		}
	}

	// Base URL for signing excludes the query string (origin + path).
	signingURL := parsed.Scheme + "://" + parsed.Host + parsed.Path
	oauthParams["oauth_signature"] = generateOAuthSignature(method, signingURL, allParams, c.creds.ConsumerSecret, c.creds.AccessTokenSecret)

	keys := make([]string, 0, len(oauthParams))
	for k := range oauthParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, percentEncode(k)+"=\""+percentEncode(oauthParams[k])+"\"")
	}
	return "OAuth " + strings.Join(parts, ", "), nil
}

func defaultNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should not fail; fall back to a timestamp-derived value.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// HTTP helper
// ---------------------------------------------------------------------------

// apiResponse is the loose envelope returned by X Ads endpoints.
type apiResponse struct {
	Data json.RawMessage `json:"data"`
	// NextCursor is set on cursor-paginated list endpoints (campaigns,
	// line_items). Empty when there are no further pages.
	NextCursor string `json:"next_cursor"`
}

// maxListPages caps how many pages a name-lookup will page through, a safety
// bound against an unexpectedly huge account (200 items/page × 25 = 5000).
const maxListPages = 25

func (c *Client) accountURL() string {
	return fmt.Sprintf("%s/%s/accounts/%s", c.baseURL, c.apiVersion, c.account.AccountID)
}

// request performs an account-scoped X Ads API request with OAuth1 signing,
// the 1-req/sec write delay handled by callers, and 429 exponential-backoff
// retry. Mirrors twitterRequest in the TS.
func (c *Client) request(ctx context.Context, method, path string, body map[string]any) (*apiResponse, error) {
	reqURL := c.accountURL() + "/" + strings.TrimPrefix(path, "/")

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
	}

	for attempt := 0; attempt <= retryMax; attempt++ {
		authHeader, err := c.buildOAuthHeader(method, reqURL, nil)
		if err != nil {
			return nil, err
		}

		var bodyReader *strings.Reader
		if bodyBytes != nil {
			bodyReader = strings.NewReader(string(bodyBytes))
		}
		var req *http.Request
		if bodyReader != nil {
			req, err = http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
		} else {
			req, err = http.NewRequestWithContext(ctx, method, reqURL, http.NoBody)
		}
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", authHeader)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("x ads api %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < retryMax {
			waitDur := c.parseRetryAfter(resp)
			_ = resp.Body.Close()
			if waitDur > 0 {
				if waitDur > 60*time.Second {
					waitDur = 60 * time.Second
				}
			} else {
				waitDur = writeDelay * time.Duration(1<<uint(attempt))
			}
			if err := sleepCtx(ctx, waitDur); err != nil {
				return nil, err
			}
			continue
		}

		respBody, _ := readAll(resp)
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("x ads api %s %s -> %d: %s", method, path, resp.StatusCode, truncate(respBody, 400))
		}

		var out apiResponse
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, &out); err != nil {
				return nil, fmt.Errorf("decode response: %w", err)
			}
		}
		return &out, nil
	}

	return nil, fmt.Errorf("x ads api %s %s -> exhausted %d retries after 429s", method, path, retryMax)
}

// parseRetryAfter returns how long to wait before retrying a 429, or 0 if no
// usable header is present. Retry-After is a delay in seconds; X-Rate-Limit-Reset
// is a Unix epoch timestamp (the X Ads API commonly returns only the latter on a
// 429), so it must be converted to a duration-until-reset rather than treated as
// a delay. Never returns a negative duration.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Rate-Limit-Reset")); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Unix(epoch, 0).Sub(c.timeFn()); d > 0 {
				return d
			}
		}
	}
	return 0
}

// sleepCtx waits for d, honoring context cancellation.
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

func readAll(resp *http.Response) ([]byte, error) {
	const maxBody = 1 << 20 // 1 MiB cap on error/response bodies.
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) >= maxBody {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// ---------------------------------------------------------------------------
// Conversion + formatting helpers
// ---------------------------------------------------------------------------

// toMicroCurrency converts USD to micro-currency (x 1,000,000), rounded.
func toMicroCurrency(usd float64) int64 {
	return int64(math.Round(usd * 1_000_000))
}

// fromMicroCurrency converts micro-currency back to USD (/ 1,000,000).
func fromMicroCurrency(micro int64) float64 {
	return float64(micro) / 1_000_000
}

// toIso8601Utc formats a YYYY-MM-DD date string as an ISO8601 UTC timestamp
// at midnight. Mirrors toIso8601Utc in the TS.
func toIso8601Utc(dateStr string) string {
	return dateStr + "T00:00:00Z"
}

// ---------------------------------------------------------------------------
// Campaign lookup (idempotency)
// ---------------------------------------------------------------------------

type campaignElement struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// findCampaignByName returns the id of a campaign matching name, or "" if not
// found. Errors are swallowed (returns "") to mirror the TS try/catch idiom.
func (c *Client) findCampaignByName(ctx context.Context, name string) string {
	return c.findByName(ctx, "campaigns?with_deleted=false", name)
}

// findLineItemByName returns the id of a line item matching name within a
// campaign, or "" if not found.
func (c *Client) findLineItemByName(ctx context.Context, campaignID, name string) string {
	return c.findByName(ctx, "line_items?campaign_id="+url.QueryEscape(campaignID)+"&with_deleted=false", name)
}

// findByName pages through a cursor-paginated X Ads list endpoint (campaigns /
// line_items) looking for an element whose name matches exactly, returning its
// id or "". It follows next_cursor so a match beyond the first page is still
// found (avoiding a duplicate create), bounded by maxListPages.
func (c *Client) findByName(ctx context.Context, path, name string) string {
	sep := "&"
	if !strings.Contains(path, "?") {
		sep = "?"
	}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		p := path
		if cursor != "" {
			p = path + sep + "cursor=" + url.QueryEscape(cursor)
		}
		resp, err := c.request(ctx, http.MethodGet, p, nil)
		if err != nil || resp == nil {
			return ""
		}
		var items []campaignElement
		if err := json.Unmarshal(resp.Data, &items); err != nil {
			return ""
		}
		for _, it := range items {
			if it.Name == name {
				return it.ID
			}
		}
		if resp.NextCursor == "" {
			return ""
		}
		cursor = resp.NextCursor
	}
	return ""
}

// ---------------------------------------------------------------------------
// Campaign name + UTM builders
// ---------------------------------------------------------------------------

func buildTwitterCampaignName(in CampaignInput) string {
	event := strings.ReplaceAll(in.EventName, "|", "-")
	project := in.Project
	if project == "" {
		project = "Linux Foundation"
	}
	project = strings.ReplaceAll(project, "|", "-")
	return fmt.Sprintf("Events | %s | Global | Awareness | Prospecting | Promoted Post | %s | MoFU", event, project)
}

var spaceRe = regexp.MustCompile(`\s+`)

func buildTwitterUtmURL(in CampaignInput) string {
	base := strings.TrimRight(in.RegistrationURL, "/")

	slug := in.EventSlug
	if slug == "" {
		slug = spaceRe.ReplaceAllString(strings.ToLower(in.EventName), "-")
	}
	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}

	params := url.Values{}
	params.Set("utm_source", "twitter")
	params.Set("utm_medium", "paid-social")
	params.Set("utm_campaign", campaign)
	params.Set("utm_term", spaceRe.ReplaceAllString(strings.ToLower(in.EventName), "-"))
	params.Set("utm_content", "promoted-tweet")

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + params.Encode()
}

// ---------------------------------------------------------------------------
// Public API — campaign creation
// ---------------------------------------------------------------------------

// CampaignInput carries the fields required to create an X Ads campaign.
// Mirrors the TS TwitterCampaignCreateRequest.
type CampaignInput struct {
	EventName       string
	EventSlug       string
	Project         string
	BudgetUsd       float64
	StartDate       string // YYYY-MM-DD
	EndDate         string // YYYY-MM-DD
	TweetID         string
	RegistrationURL string
	HSToken         string
}

// CampaignResult is the outcome of a campaign creation attempt, including a
// step-by-step log. Mirrors the TS TwitterCampaignCreateResult.
type CampaignResult struct {
	Platform        string
	CampaignName    string
	CampaignID      string
	LineItemName    string
	LineItemID      string
	PromotedTweetID string
	TwitterURL      string
	Steps           []string
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// CreateCampaign runs the campaign -> line_item -> promoted_tweet creation
// flow, reusing existing entities by name for idempotency. It mirrors
// executeTwitterCampaignCreation in the TS. Everything is created PAUSED.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	steps := []string{}

	// Validate.
	if math.IsNaN(in.BudgetUsd) || math.IsInf(in.BudgetUsd, 0) || in.BudgetUsd <= 0 {
		return nil, fmt.Errorf("invalid budget: must be a positive number")
	}
	if !dateRe.MatchString(in.StartDate) {
		return nil, fmt.Errorf("invalid start date format: %s — expected YYYY-MM-DD", in.StartDate)
	}
	if !dateRe.MatchString(in.EndDate) {
		return nil, fmt.Errorf("invalid end date format: %s — expected YYYY-MM-DD", in.EndDate)
	}
	if in.EndDate <= in.StartDate {
		return nil, fmt.Errorf("end date %s must be after start date %s", in.EndDate, in.StartDate)
	}

	// Step 1: verify account (non-fatal).
	c.verifyAccount(ctx, &steps)

	// Step 2: create campaign (PAUSED), reusing by name.
	campaignName := buildTwitterCampaignName(in)
	campaignID := c.findCampaignByName(ctx, campaignName)
	if campaignID != "" {
		steps = append(steps, fmt.Sprintf("Reusing existing campaign: %s", campaignID))
	} else {
		campaignBody := map[string]any{
			"name":                            campaignName,
			"funding_instrument_id":           c.account.FundingInstrumentID,
			"start_time":                      toIso8601Utc(in.StartDate),
			"end_time":                        toIso8601Utc(in.EndDate),
			"daily_budget_amount_local_micro": toMicroCurrency(in.BudgetUsd),
			"paused":                          true,
		}
		if err := sleepCtx(ctx, writeDelay); err != nil {
			return nil, err
		}
		resp, err := c.request(ctx, http.MethodPost, "campaigns", campaignBody)
		if err != nil {
			return nil, err
		}
		campaignID = extractID(resp)
		if campaignID == "" {
			return nil, fmt.Errorf("x campaign creation succeeded but returned no campaign ID")
		}
		steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, $%.2f/day)", campaignID, in.BudgetUsd))
	}

	// Step 3: create line item (ad group), reusing by name.
	lineItemName := fmt.Sprintf("Events | %s | Promoted Tweets | AUTO", strings.ReplaceAll(in.EventName, "|", "-"))
	lineItemID := c.findLineItemByName(ctx, campaignID, lineItemName)
	if lineItemID != "" {
		steps = append(steps, fmt.Sprintf("Reusing existing line item: %s", lineItemID))
	} else {
		lineItemBody := map[string]any{
			"campaign_id":  campaignID,
			"name":         lineItemName,
			"product_type": "PROMOTED_TWEETS",
			"placements":   "ALL_ON_TWITTER",
			"objective":    "WEBSITE_CLICKS",
			"bid_type":     "AUTO",
			"paused":       true,
		}
		if err := sleepCtx(ctx, writeDelay); err != nil {
			return nil, err
		}
		resp, err := c.request(ctx, http.MethodPost, "line_items", lineItemBody)
		if err != nil {
			return nil, err
		}
		lineItemID = extractID(resp)
		if lineItemID == "" {
			return nil, fmt.Errorf("x line item creation succeeded but returned no line item ID")
		}
		steps = append(steps, fmt.Sprintf("Line item created: %s (PAUSED, ALL_ON_TWITTER, AUTO bid)", lineItemID))
	}

	// Step 4: create promoted tweet if a tweet ID was provided.
	var promotedTweetID string
	if in.TweetID != "" {
		if err := sleepCtx(ctx, writeDelay); err != nil {
			return nil, err
		}
		resp, err := c.request(ctx, http.MethodPost, "promoted_tweets", map[string]any{
			"line_item_id": lineItemID,
			"tweet_ids":    in.TweetID,
		})
		if err != nil {
			steps = append(steps, fmt.Sprintf("Promoted tweet creation failed: %s — add manually in X Ads Manager", err.Error()))
		} else {
			promotedTweetID = extractPromotedTweetID(resp)
			if promotedTweetID != "" {
				steps = append(steps, fmt.Sprintf("Promoted tweet created: %s (tweet: %s)", promotedTweetID, in.TweetID))
			}
		}
	} else {
		utmURL := buildTwitterUtmURL(in)
		steps = append(steps, "No tweet ID provided — post a tweet manually, then add it as a promoted tweet in X Ads Manager")
		steps = append(steps, fmt.Sprintf("Destination URL with UTM: %s", utmURL))
	}

	return &CampaignResult{
		Platform:        "twitter-ads",
		CampaignName:    campaignName,
		CampaignID:      campaignID,
		LineItemName:    lineItemName,
		LineItemID:      lineItemID,
		PromotedTweetID: promotedTweetID,
		TwitterURL:      AdsManagerURL,
		Steps:           steps,
	}, nil
}

// verifyAccount performs a best-effort account lookup, appending a step. All
// failures are non-fatal (mirrors the TS Step 1 try/catch).
func (c *Client) verifyAccount(ctx context.Context, steps *[]string) {
	verifyURL := c.accountURL()
	authHeader, err := c.buildOAuthHeader(http.MethodGet, verifyURL, nil)
	if err != nil {
		*steps = append(*steps, fmt.Sprintf("Account verification warning: %s", err.Error()))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, http.NoBody)
	if err != nil {
		*steps = append(*steps, fmt.Sprintf("Account verification warning: %s", err.Error()))
		return
	}
	req.Header.Set("Authorization", authHeader)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		*steps = append(*steps, fmt.Sprintf("Account verification warning: %s", err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		*steps = append(*steps, fmt.Sprintf("Account verification returned %d", resp.StatusCode))
		return
	}
	body, _ := readAll(resp)
	var parsed struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	name := c.account.AccountID
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Data.Name != "" {
		name = parsed.Data.Name
	}
	*steps = append(*steps, fmt.Sprintf("Account verified: %s", name))
}

// extractID reads data.id from a response envelope.
func extractID(resp *apiResponse) string {
	if resp == nil || len(resp.Data) == 0 {
		return ""
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &obj); err == nil {
		return obj.ID
	}
	return ""
}

// extractPromotedTweetID reads the promoted tweet id, which the X Ads API
// returns as an array (data[0].id) or occasionally a single object.
func extractPromotedTweetID(resp *apiResponse) string {
	if resp == nil || len(resp.Data) == 0 {
		return ""
	}
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &arr); err == nil {
		if len(arr) > 0 {
			return arr[0].ID
		}
		return ""
	}
	return extractID(resp)
}
