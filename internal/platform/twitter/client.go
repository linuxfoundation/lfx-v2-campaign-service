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
	"unicode/utf8"
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
	// maxBudgetUsd caps the budget well below the int64 micro-unit overflow
	// threshold (int64 max / 1e6 ≈ 9.2e12) so the ×1e6 conversion in
	// toMicroCurrency can never wrap to a negative value. Mirrors the reddit
	// client's redditMaxBudgetUSD.
	maxBudgetUsd = 1_000_000_000.0
	// maxEventNameLen bounds the event name folded into campaign / line-item
	// names, guarding against unbounded input producing oversized API payloads.
	maxEventNameLen = 200
	// maxProjectLen bounds the project name folded into the campaign name. Like
	// EventName, Project is caller-supplied and otherwise unbounded, so it is
	// trimmed and length-capped before composition.
	maxProjectLen = 200
	// maxEntityNameLen is X's hard limit on a campaign / line-item entity name.
	// The composed name (event + project + fixed template) can exceed this even
	// when EventName and Project are individually within bounds, so the FINAL
	// composed names are validated against this rune limit before any create call.
	maxEntityNameLen = 255
	// retryMax mirrors TWITTER_API_RETRY_MAX.
	retryMax = 3
	// maxRetryWait caps how long a single 429 backoff will sleep. X rate-limit
	// windows can be far longer than a request is willing to wait; if the
	// server-declared reset exceeds this cap we abort with the rate-limit error
	// instead of sleeping pointlessly (and a hostile huge reset can't hang us).
	maxRetryWait = 90 * time.Second
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

	// writeDelay paces sequential write requests within a single dispatch
	// (Twitter allows ~1 write/sec). Injectable so tests can set it to 0 rather
	// than incurring real per-request sleeps; defaults to the writeDelay const.
	writeDelay time.Duration
}

// Option customizes a Client at construction time.
type Option func(*Client)

// WithBaseURL overrides the API base URL (default DefaultBaseURL).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithAPIVersion overrides the API version segment (default DefaultAPIVersion).
func WithAPIVersion(v string) Option { return func(c *Client) { c.apiVersion = v } }

// WithHTTPClient overrides the underlying *http.Client (default has a 30s
// timeout). A nil client is ignored so the option can't produce an unusable
// Client whose httpClient.Do would panic.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// WithWriteDelay overrides the inter-write pacing delay. A zero (or negative)
// value disables the pacing sleep entirely — useful in tests to avoid real
// per-request sleeps.
func WithWriteDelay(d time.Duration) Option { return func(c *Client) { c.writeDelay = d } }

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
		writeDelay: writeDelay,
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
	// OAuth 1.0a (RFC 5849 §3.4.1.3.2) normalizes parameters by their
	// PERCENT-ENCODED name, breaking ties on the percent-encoded value — not by
	// the raw key. Sorting raw keys is wrong: e.g. "c@" encodes to "c%40" and
	// must sort BEFORE "c2" because '%' (0x25) < '2' (0x32), yet raw '@' (0x40)
	// sorts AFTER '2'. Encode first, then sort by (name, value) as a TUPLE.
	//
	// Sorting the joined "name=value" string is ALSO wrong: it misorders when one
	// encoded name is a prefix of another. Names "a" and "a1" must order a < a1,
	// but "a1=<v>" sorts BEFORE "a=<v>" on the joined form because '1' (0x31) <
	// '=' (0x3D). Compare names first, then values as a tiebreak.
	type encodedPair struct{ name, value string }
	pairs := make([]encodedPair, 0, len(params))
	for k, v := range params {
		pairs = append(pairs, encodedPair{percentEncode(k), percentEncode(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.name+"="+p.value)
	}
	paramString := strings.Join(parts, "&")

	baseString := strings.ToUpper(method) + "&" + percentEncode(u) + "&" + percentEncode(paramString)
	signingKey := percentEncode(consumerSecret) + "&" + percentEncode(tokenSecret)

	mac := hmac.New(sha1.New, []byte(signingKey))
	mac.Write([]byte(baseString))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildOAuthHeader builds the "Authorization: OAuth ..." header for a request.
// Query parameters present on rawURL are folded into the OAuth 1.0a signature
// base string (X Ads create calls carry their params on the query string).
// bodyParams is retained for callers that sign extra form params; callers here
// pass nil since no request carries a body.
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

// request performs an account-scoped X Ads API GET/list request with OAuth1
// signing and 429 exponential-backoff retry. Any parameters must be encoded
// into path as a query string. Mirrors twitterRequest in the TS for reads.
func (c *Client) request(ctx context.Context, method, path string) (*apiResponse, error) {
	return c.doRequest(ctx, method, path, nil)
}

// createRequest performs an X Ads API create (POST) call. Per the X Ads v12
// contract, create endpoints (campaigns, line_items, promoted_tweets) accept
// their parameters as URL query parameters, not a JSON body. The params are
// appended to the request URL and also folded into the OAuth signature base
// string (OAuth 1.0a signs query params), and the request is sent with no
// body. Callers own the 1-req/sec write delay.
func (c *Client) createRequest(ctx context.Context, path string, params map[string]string) (*apiResponse, error) {
	return c.doRequest(ctx, http.MethodPost, path, params)
}

// doRequest is the shared HTTP path with OAuth1 signing and 429
// exponential-backoff retry. queryParams, when non-nil, are appended to the
// request URL (create calls pass their params here); the request carries no
// body in either mode.
func (c *Client) doRequest(ctx context.Context, method, path string, queryParams map[string]string) (*apiResponse, error) {
	reqURL := c.accountURL() + "/" + strings.TrimPrefix(path, "/")

	if len(queryParams) > 0 {
		vals := url.Values{}
		for k, v := range queryParams {
			vals.Set(k, v)
		}
		sep := "?"
		if strings.Contains(reqURL, "?") {
			sep = "&"
		}
		reqURL += sep + vals.Encode()
	}

	for attempt := 0; attempt <= retryMax; attempt++ {
		authHeader, err := c.buildOAuthHeader(method, reqURL, nil)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", authHeader)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("x ads api %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			// If this was the last attempt, don't sleep+retry: the loop would
			// exit and the 429 would otherwise fall through to the generic
			// non-2xx return below. Surface the intended exhausted-rate-limit
			// error instead.
			if attempt >= retryMax {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("x ads api %s %s -> exhausted %d retries after 429s", method, path, retryMax)
			}
			waitDur := c.parseRetryAfter(resp)
			_ = resp.Body.Close()
			if waitDur > 0 {
				// The server declared a reset time (Retry-After delay or
				// X-Rate-Limit-Reset epoch). Honor it rather than clamping to a
				// small value and burning every retry while still limited. If the
				// wait exceeds our cap, sleeping would consume a retry without any
				// chance of the window clearing, so abort with the rate-limit error.
				if waitDur > maxRetryWait {
					return nil, fmt.Errorf("x ads api %s %s -> 429: rate-limit reset in %s exceeds max wait %s; aborting", method, path, waitDur.Round(time.Second), maxRetryWait)
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

// pace waits c.writeDelay between sequential write requests within a single
// dispatch, honoring context cancellation. A non-positive writeDelay disables
// the sleep (used by tests).
func (c *Client) pace(ctx context.Context) error {
	if c.writeDelay <= 0 {
		return nil
	}
	return sleepCtx(ctx, c.writeDelay)
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

// findCampaignByName returns the id of a campaign matching name, or "" if no
// such campaign exists. A non-nil error signals a transient/unexpected lookup
// failure (failed GET or undecodable response) — the caller must abort rather
// than treat it as "not found" and create a duplicate.
func (c *Client) findCampaignByName(ctx context.Context, name string) (string, error) {
	return c.findByName(ctx, "campaigns?with_deleted=false", name)
}

// findLineItemByName returns the id of a line item matching name within a
// campaign, or "" if none exists. A non-nil error signals a lookup failure the
// caller must not swallow (see findCampaignByName).
func (c *Client) findLineItemByName(ctx context.Context, campaignID, name string) (string, error) {
	// The X Ads list endpoint filters line items with campaign_ids (plural);
	// campaign_id (singular) is the CREATE parameter. Using the singular key here
	// would leave the lookup unscoped and could reuse a same-named line item from
	// another campaign.
	return c.findByName(ctx, "line_items?campaign_ids="+url.QueryEscape(campaignID)+"&with_deleted=false", name)
}

// findByName pages through a cursor-paginated X Ads list endpoint (campaigns /
// line_items) looking for an element whose name matches exactly. It returns
// (id, nil) on a match, ("", nil) for a genuine not-found (the pages were read
// successfully but held no match), and ("", err) when a page GET or decode
// fails — so a transient error is never conflated with "not found" and the
// caller can abort instead of creating a duplicate. A name match whose element
// carries no usable id is likewise returned as ("", err), not ("", nil), so the
// caller does not follow with a create and duplicate an existing element. It
// follows next_cursor so a match beyond the first page is still found, bounded
// by maxListPages.
func (c *Client) findByName(ctx context.Context, path, name string) (string, error) {
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
		resp, err := c.request(ctx, http.MethodGet, p)
		if err != nil {
			return "", fmt.Errorf("lookup %q: %w", name, err)
		}
		if resp == nil {
			return "", fmt.Errorf("lookup %q: empty response", name)
		}
		var items []campaignElement
		if err := json.Unmarshal(resp.Data, &items); err != nil {
			return "", fmt.Errorf("lookup %q: decode list: %w", name, err)
		}
		for _, it := range items {
			if it.Name == name {
				// A match with no usable id cannot be reused. Returning ("", nil)
				// here would read as "not found" and drive the caller into a create
				// POST, risking a duplicate of an element that already exists.
				// Surface it as a lookup error so the caller aborts instead.
				if it.ID == "" {
					return "", fmt.Errorf("lookup %q: matching element has no id; aborting to avoid creating a duplicate", name)
				}
				return it.ID, nil
			}
		}
		if resp.NextCursor == "" {
			return "", nil
		}
		cursor = resp.NextCursor
	}
	// Hit the page cap with a cursor still outstanding: we can't be sure the name
	// doesn't exist further on, so return an error rather than "not found" (which
	// would let the caller create a duplicate).
	return "", fmt.Errorf("lookup %q: exceeded %d pages with more results remaining; aborting to avoid creating a duplicate", name, maxListPages)
}

// ---------------------------------------------------------------------------
// Campaign name + UTM builders
// ---------------------------------------------------------------------------

func buildTwitterCampaignName(in CampaignInput) string {
	event := strings.ReplaceAll(in.EventName, "|", "-")
	project := boundProject(in.Project)
	project = strings.ReplaceAll(project, "|", "-")
	return fmt.Sprintf("Events | %s | Global | Awareness | Prospecting | Promoted Post | %s | MoFU", event, project)
}

// boundProject trims the caller-supplied project name and caps its rune length,
// defaulting to the canonical Linux Foundation project slug "tlf" when empty.
// The data pipeline parses the campaign name for attribution and joins on the
// canonical slug ("tlf", lowercase — not a display name); see docs/api-catalog.md.
// Project is otherwise unbounded, so bounding it here keeps the composed campaign
// name from ballooning.
func boundProject(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return "tlf"
	}
	if r := []rune(project); len(r) > maxProjectLen {
		project = string(r[:maxProjectLen])
	}
	return project
}

// validateEntityName enforces X's 255-rune entity-name limit on a FINAL composed
// campaign / line-item name. Even with EventName and Project individually bounded,
// the composed name (event + project + fixed template) can exceed 255, so it is
// checked here before any create call. kind is "campaign" or "line item".
func validateEntityName(kind, name string) error {
	if n := len([]rune(name)); n > maxEntityNameLen {
		return fmt.Errorf("invalid %s name: composed name is %d characters, exceeds X's %d-character limit", kind, n, maxEntityNameLen)
	}
	return nil
}

var spaceRe = regexp.MustCompile(`\s+`)

func buildTwitterUtmURL(in CampaignInput) string {
	slug := in.EventSlug
	if slug == "" {
		slug = spaceRe.ReplaceAllString(strings.ToLower(in.EventName), "-")
	}
	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}

	raw := strings.TrimSpace(in.RegistrationURL)
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable URL: return it unchanged rather than corrupting it.
		return raw
	}
	// Merge UTM params into the URL's existing query and re-render, so the query
	// lands before any fragment (a naive string append would put it inside the
	// fragment, e.g. https://x/reg#a?utm_...).
	q := u.Query()
	q.Set("utm_source", "twitter")
	q.Set("utm_medium", "paid-social")
	q.Set("utm_campaign", campaign)
	q.Set("utm_term", spaceRe.ReplaceAllString(strings.ToLower(in.EventName), "-"))
	q.Set("utm_content", "promoted-tweet")
	u.RawQuery = q.Encode()
	return u.String()
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
	// PromotedTweetWarning is non-empty when the promoted-tweet association could
	// not be confirmed (POST failed, or returned a malformed/empty response). The
	// campaign and line item may still have been created, so the overall call is
	// not fatal, but consumers MUST NOT treat a result with this set as an
	// unqualified success — the promoted tweet may need to be added manually.
	PromotedTweetWarning string
	TwitterURL           string
	Steps                []string
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// validateDate enforces both the YYYY-MM-DD shape and that the value is a real
// calendar date. The regex alone accepts impossible dates like "2026-99-99",
// which would be forwarded as a bogus ISO8601 timestamp to the X Ads API; a
// strict time.Parse (which rejects out-of-range months/days) closes that gap
// before any mutating call. label is "start" or "end" for the error message.
func validateDate(label, date string) error {
	if !dateRe.MatchString(date) {
		return fmt.Errorf("invalid %s date format: %s — expected YYYY-MM-DD", label, date)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return fmt.Errorf("invalid %s date: %s is not a real calendar date", label, date)
	}
	return nil
}

// CreateCampaign runs the campaign -> line_item -> promoted_tweet creation
// flow, reusing existing entities by name for idempotency. It mirrors
// executeTwitterCampaignCreation in the TS. The campaign and line item are
// created PAUSED (entity_status=PAUSED); the promoted-tweet association is
// created ACTIVE by the API (the endpoint does not accept entity_status), but
// the paused line item gates delivery so nothing serves until it is enabled.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	steps := []string{}

	// Validate EventName before any mutating call: an empty/whitespace value
	// would produce identical generic campaign & line-item names for every such
	// request, letting the find-by-name lookup silently reuse an unrelated
	// campaign. Trim and normalize it up front so every downstream builder sees
	// the cleaned value.
	in.EventName = strings.TrimSpace(in.EventName)
	if in.EventName == "" {
		return nil, fmt.Errorf("invalid event name: must not be empty")
	}
	if utf8.RuneCountInString(in.EventName) > maxEventNameLen {
		return nil, fmt.Errorf("invalid event name: exceeds %d characters", maxEventNameLen)
	}

	// Validate budget. Reject NaN/Inf/non-positive, reject values above the
	// int64 micro-unit overflow cap, and reject anything that rounds to zero (or
	// negative) micro-units — such a value passes a naive >0 check but would send
	// a zero/negative daily_budget_amount_local_micro.
	if math.IsNaN(in.BudgetUsd) || math.IsInf(in.BudgetUsd, 0) || in.BudgetUsd <= 0 {
		return nil, fmt.Errorf("invalid budget: must be a positive number")
	}
	if in.BudgetUsd > maxBudgetUsd {
		return nil, fmt.Errorf("invalid budget: must be at most %v", maxBudgetUsd)
	}
	if toMicroCurrency(in.BudgetUsd) <= 0 {
		return nil, fmt.Errorf("invalid budget: %g rounds to zero micro-units", in.BudgetUsd)
	}
	if err := validateDate("start", in.StartDate); err != nil {
		return nil, err
	}
	if err := validateDate("end", in.EndDate); err != nil {
		return nil, err
	}
	if in.EndDate <= in.StartDate {
		return nil, fmt.Errorf("end date %s must be after start date %s", in.EndDate, in.StartDate)
	}

	// Validate required account config before any mutating call. account_id and
	// funding_instrument_id are both required by the X Ads campaign-create
	// contract, but a stored connection may persist them as empty strings (the
	// connection contract permits funding_instrument_id to be omitted). Failing
	// fast client-side yields a clear error instead of an opaque X API rejection
	// (and never issues a create with a missing funding instrument).
	if strings.TrimSpace(c.account.AccountID) == "" {
		return nil, fmt.Errorf("invalid account config: account_id must not be empty")
	}
	if strings.TrimSpace(c.account.FundingInstrumentID) == "" {
		return nil, fmt.Errorf("invalid account config: funding_instrument_id must not be empty")
	}

	// Compose and validate the entity names before ANY network call: even with
	// EventName and Project individually bounded, the composed campaign / line-item
	// names can exceed X's 255-rune entity-name limit, so reject an oversized name
	// up front rather than after a wasted account-verify / lookup round trip.
	campaignName := buildTwitterCampaignName(in)
	if err := validateEntityName("campaign", campaignName); err != nil {
		return nil, err
	}
	lineItemName := fmt.Sprintf("Events | %s | Promoted Tweets | AUTO", strings.ReplaceAll(in.EventName, "|", "-"))
	if err := validateEntityName("line item", lineItemName); err != nil {
		return nil, err
	}

	// Step 1: verify account (non-fatal).
	c.verifyAccount(ctx, &steps)

	// Step 2: create campaign (PAUSED), reusing by name.
	campaignID, err := c.findCampaignByName(ctx, campaignName)
	if err != nil {
		return nil, err
	}
	if campaignID != "" {
		steps = append(steps, fmt.Sprintf("Reusing existing campaign: %s", campaignID))
	} else {
		// X Ads v12 create endpoints take parameters as URL query params (not a
		// JSON body), and use entity_status=PAUSED (not paused=true). Note: the
		// campaign endpoint does NOT accept start_time/end_time in v12 — flight
		// dates belong on the line item (sent below); including them here gets the
		// campaign create rejected.
		campaignParams := map[string]string{
			"name":                            campaignName,
			"funding_instrument_id":           c.account.FundingInstrumentID,
			"daily_budget_amount_local_micro": strconv.FormatInt(toMicroCurrency(in.BudgetUsd), 10),
			"entity_status":                   "PAUSED",
		}
		// These inter-request sleeps pace THIS dispatch's own sequential writes
		// (campaign -> line item -> promoted tweet) to stay under X's per-second
		// write rate. They do NOT enforce X's account-wide write limit across
		// concurrent or replicated dispatches: this service dispatches jobs async
		// (possibly across replicas), and separately-constructed clients in
		// different goroutines/processes can wake and POST at the same instant.
		// Correct account-wide limiting needs shared cross-replica coordination
		// (a distributed limiter or the orchestrator serializing per account),
		// which is out of scope for this stateless per-request client and is
		// tracked by LFXV2-2665 (durable dispatch). If the account limit is hit
		// anyway, the 429 exponential-backoff retry in doRequest is the backstop.
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		resp, err := c.createRequest(ctx, "campaigns", campaignParams)
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
	lineItemID, err := c.findLineItemByName(ctx, campaignID, lineItemName)
	if err != nil {
		return nil, err
	}
	if lineItemID != "" {
		steps = append(steps, fmt.Sprintf("Reusing existing line item: %s", lineItemID))
	} else {
		// X Ads v12 line_items: params go on the query string; start_time and
		// end_time are REQUIRED; bid_strategy=AUTO selects automatic bidding
		// (the field is bid_strategy in v12, not bid_type); entity_status
		// replaces the removed paused flag.
		lineItemParams := map[string]string{
			"campaign_id":   campaignID,
			"name":          lineItemName,
			"product_type":  "PROMOTED_TWEETS",
			"placements":    "ALL_ON_TWITTER",
			"objective":     "WEBSITE_CLICKS",
			"bid_strategy":  "AUTO",
			"start_time":    toIso8601Utc(in.StartDate),
			"end_time":      toIso8601Utc(in.EndDate),
			"entity_status": "PAUSED",
		}
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		resp, err := c.createRequest(ctx, "line_items", lineItemParams)
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
	var promotedTweetWarning string
	if in.TweetID != "" {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		// The promoted_tweets endpoint does not accept entity_status; the API
		// creates the association ACTIVE. Delivery is still gated by the PAUSED
		// line item above, so we intentionally send only the association params.
		// This POST is always re-issued on a repeated CreateCampaign (unlike the
		// find-or-create campaign/line-item steps), so a lost first response can
		// make the retry hit a duplicate — handled below.
		resp, err := c.createRequest(ctx, "promoted_tweets", map[string]string{
			"line_item_id": lineItemID,
			"tweet_ids":    in.TweetID,
		})
		switch {
		case err != nil && isDuplicatePromotedTweetErr(err):
			// The association already exists (e.g. a prior POST that succeeded but
			// whose response was lost). Idempotent: treat as success, not a gap.
			steps = append(steps, fmt.Sprintf("Promoted tweet already associated with line item %s (tweet: %s) — treating as created (idempotent)", lineItemID, in.TweetID))
		case err != nil:
			// A real POST failure. Do NOT report unqualified success: record a
			// warning both in the step log and on the result so the caller can see
			// the promoted tweet may not have been created/associated.
			promotedTweetWarning = fmt.Sprintf("promoted-tweet POST failed for tweet %s: %s", in.TweetID, err.Error())
			steps = append(steps, fmt.Sprintf("Promoted tweet creation failed: %s — add manually in X Ads Manager", err.Error()))
		default:
			promotedTweetID = extractPromotedTweetID(resp)
			if promotedTweetID != "" {
				steps = append(steps, fmt.Sprintf("Promoted tweet created: %s (tweet: %s; created ACTIVE by the API but held from serving by the PAUSED line item)", promotedTweetID, in.TweetID))
			} else {
				// A 2xx response missing data.id is a malformed success: don't
				// silently treat it as done. Surface a warning (step + result field)
				// so the gap is visible without making the whole flow fatal.
				promotedTweetWarning = fmt.Sprintf("promoted-tweet POST returned no ID (malformed response) for tweet %s", in.TweetID)
				steps = append(steps, fmt.Sprintf("Promoted tweet creation returned no promoted-tweet ID (malformed response, tweet: %s) — add it manually in X Ads Manager", in.TweetID))
			}
		}
	} else {
		utmURL := buildTwitterUtmURL(in)
		steps = append(steps, "No tweet ID provided — post a tweet manually, then add it as a promoted tweet in X Ads Manager")
		steps = append(steps, fmt.Sprintf("Destination URL with UTM: %s", utmURL))
	}

	return &CampaignResult{
		Platform:             "twitter-ads",
		CampaignName:         campaignName,
		CampaignID:           campaignID,
		LineItemName:         lineItemName,
		LineItemID:           lineItemID,
		PromotedTweetID:      promotedTweetID,
		PromotedTweetWarning: promotedTweetWarning,
		TwitterURL:           AdsManagerURL,
		Steps:                steps,
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

// isDuplicatePromotedTweetErr reports whether err from a promoted_tweets POST is
// X's "this tweet is already promoted on this line item" rejection. On a repeated
// CreateCampaign the campaign and line item are reused by name, but the
// promoted-tweet association is always re-POSTed; if the first POST's response was
// lost, the retry hits this duplicate. Because doRequest surfaces non-2xx bodies
// as the error string, we pattern-match X's recognizable duplicate signals
// (error code DUPLICATE_PROMOTABLE_ENTITY / message wording). When it matches the
// association already exists, so we treat it as idempotent success rather than a
// failure or a false unqualified success. NOTE: true cross-call idempotency
// (idempotency keys sent to X) is tracked in LFXV2-2665.
func isDuplicatePromotedTweetErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "duplicate_promotable_entity") ||
		strings.Contains(s, "already promoted") ||
		strings.Contains(s, "already associated") ||
		(strings.Contains(s, "duplicate") && strings.Contains(s, "promot"))
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
