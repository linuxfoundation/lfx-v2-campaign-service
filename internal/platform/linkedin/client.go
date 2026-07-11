// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client is a standalone LinkedIn Marketing API client. Construct it with
// NewClient. The client holds no mutable state and its methods are safe to call
// concurrently, provided the injected RuntimeConfig (its slices/maps) is not
// mutated by the caller after construction.
type Client struct {
	creds      Credentials
	cfg        RuntimeConfig
	httpClient *http.Client
	baseURL    string
	apiVersion string
	// now allows tests to control the clock. Defaults to time.Now.
	now func() time.Time
	// retryBaseDelay is the base for exponential 429 backoff. Defaults to the
	// retryBaseDelay const; tests may shrink it to keep runs fast.
	retryBaseDelay time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient overrides the default *http.Client (30s timeout).
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

// NewClient builds a Client from injected credentials and runtime config.
// The package never reads env vars or files; everything comes through here.
func NewClient(creds Credentials, cfg RuntimeConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		cfg:            cfg,
		httpClient:     &http.Client{Timeout: requestTimeout},
		baseURL:        baseURL,
		apiVersion:     apiVersion,
		now:            time.Now,
		retryBaseDelay: retryBaseDelay,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// Local request / result types (mirror the TS request/result)
// ---------------------------------------------------------------------------

// CreativeVariant is one ad variant. Mirrors LinkedInCreativeVariant.
type CreativeVariant struct {
	IntroText string
	Headline  string
	ImageURN  string // optional
}

// CampaignInput is the full campaign-creation request. Mirrors
// LinkedInCampaignCreateRequest (the fields CreateCampaign consumes).
type CampaignInput struct {
	EventName        string
	RegistrationURL  string
	HSToken          string // optional
	BudgetUSD        float64
	LifetimeBudget   bool
	StartDate        string // YYYY-MM-DD
	EndDate          string // YYYY-MM-DD
	GeoTargets       []GeoTarget
	TargetingProfile string
	Variants         []CreativeVariant
	Project          string // optional; defaults to "TLF"
	// AdAccountID optionally overrides the default account. Must be in the
	// runtime config's accounts list when set.
	AdAccountID string
}

// CampaignResult is the outcome of CreateCampaign. Mirrors
// LinkedInCampaignCreateResult.
type CampaignResult struct {
	Platform          string   `json:"platform"`
	CampaignGroupName string   `json:"campaignGroupName"`
	CampaignGroupID   string   `json:"campaignGroupId"`
	CampaignName      string   `json:"campaignName"`
	CampaignID        string   `json:"campaignId"`
	CreativeCount     int      `json:"creativeCount"`
	LinkedInURL       string   `json:"linkedInUrl"`
	Steps             []string `json:"steps"`
}

// ---------------------------------------------------------------------------
// HTTP layer
// ---------------------------------------------------------------------------

// linkedInResponse is the decoded JSON body plus the resource ID promoted from
// the x-restli-id header. Mirrors LinkedInResponse.
type linkedInResponse struct {
	ID       flexibleID        `json:"id"`
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Elements []responseElement `json:"elements"`
	Metadata linkedInMetadata  `json:"metadata"`
}

// linkedInMetadata carries the cursor-pagination block used by the LinkedIn
// search APIs at LinkedIn-Version 202602: the response advertises the next
// page via metadata.nextPageToken, which the client echoes back as the
// `pageToken` request param. An empty nextPageToken means the result set is
// exhausted.
type linkedInMetadata struct {
	NextPageToken string `json:"nextPageToken"`
}

// flexibleID decodes a LinkedIn resource identifier that the API returns as
// EITHER a JSON number (a long, e.g. campaign/campaign-group search results) or
// a JSON string (e.g. a URN like "urn:li:sponsoredCampaign:200"). Both forms are
// normalized to their string representation. Decoding the numeric form into a Go
// string previously failed json.Unmarshal outright, silently breaking search
// once a real numeric id appeared.
type flexibleID string

// UnmarshalJSON accepts a JSON number or a JSON string and yields the string
// form. A JSON null (or absent field) decodes to the empty string.
func (f *flexibleID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*f = ""
		return nil
	}
	// Quoted string form: unquote to strip the JSON escaping.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexibleID(s)
		return nil
	}
	// Numeric form (a long): keep the exact digits via json.Number so a large id
	// is never distorted by float64 rounding.
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return fmt.Errorf("resource id is neither a JSON string nor number: %w", err)
	}
	*f = flexibleID(n.String())
	return nil
}

// String returns the normalized string form of the id.
func (f flexibleID) String() string { return string(f) }

// responseElement mirrors LinkedInResponseElement. LinkedIn returns an
// element's identifier under any of `id`, `$URN`, or `urn` depending on the
// endpoint, so each is decoded into its own field and the read sites fall back
// through ID → DURN → URN. The `id` field is a flexibleID because search
// results return it as a numeric long while other endpoints return a quoted URN.
type responseElement struct {
	Name   string     `json:"name"`
	Status string     `json:"status"`
	ID     flexibleID `json:"id"`
	URN    string     `json:"urn"`
	DURN   string     `json:"$URN"`
	// CampaignGroup is the parent campaign-group URN a campaign belongs to
	// (e.g. "urn:li:sponsoredCampaignGroup:123"). It is only populated for
	// campaign search results and is used to scope the find-existing-campaign
	// lookup to the resolved group, so a same-name campaign under a DIFFERENT
	// (e.g. archived/replaced) group is not treated as a match.
	CampaignGroup string `json:"campaignGroup"`
}

var pathValidRE = regexp.MustCompile(`^[a-zA-Z0-9/_:?=&.-]*$`)

// apiError is a non-2xx HTTP response from the LinkedIn API. It carries the
// status code so callers (e.g. findByName) can distinguish a 404 "not found"
// from a transient/unexpected failure that must not be swallowed.
type apiError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("LinkedIn API %s %s -> %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// doRequest performs one API call. It honors ctx, sets the OAuth2 bearer and
// LinkedIn headers, applies the client timeout, and promotes x-restli-id into
// the returned ID. Mirrors linkedInRequest().
func (c *Client) doRequest(ctx context.Context, method, path string, body map[string]any, params map[string]string) (*linkedInResponse, error) {
	sanitized := strings.TrimPrefix(path, "/")
	if !pathValidRE.MatchString(sanitized) || strings.Contains(sanitized, "..") {
		return nil, fmt.Errorf("invalid LinkedIn API path: %q", sanitized)
	}

	u, err := url.Parse(c.baseURL + "/" + sanitized)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if len(params) > 0 {
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	var encoded []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		encoded = b
	}

	// A 429 is retried up to retryMax times with a bounded backoff (honoring
	// Retry-After when present), since CreateCampaign drives several sequential
	// Marketing API calls (campaign group, campaign, dark post, creative) that
	// can trip a per-account rate limit mid-flow.
	for attempt := 0; attempt <= retryMax; attempt++ {
		var reqBody *bytes.Reader
		if encoded != nil {
			reqBody = bytes.NewReader(encoded)
		} else {
			reqBody = bytes.NewReader(nil)
		}

		req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
		req.Header.Set("LinkedIn-Version", c.apiVersion)
		req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("linkedin %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < retryMax {
			wait := c.parseRetryAfter(resp)
			_ = resp.Body.Close()
			if wait <= 0 {
				wait = c.retryBaseDelay * time.Duration(1<<uint(attempt))
			}
			if wait > maxRetryWait {
				wait = maxRetryWait
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		// Bound the response body read so an unexpectedly large response can't
		// exhaust memory (10 MiB is far above any legitimate LinkedIn API response).
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes)); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("read response body: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			text := buf.String()
			if len(text) > 400 {
				text = text[:400]
			}
			_ = resp.Body.Close()
			return nil, &apiError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: text}
		}

		out := &linkedInResponse{}
		if strings.Contains(resp.Header.Get("Content-Type"), "application/json") && buf.Len() > 0 {
			if err := json.Unmarshal(buf.Bytes(), out); err != nil {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("decode response: %w", err)
			}
		}

		// Promote the resource ID header when the body carried no id. Mirrors the
		// x-restli-id fallback in linkedInRequest().
		if out.ID == "" {
			// http.Header.Get canonicalizes the key, so a single lookup covers any
			// casing the server used (x-restli-id / X-RestLi-Id → X-Restli-Id).
			if rid := resp.Header.Get("x-restli-id"); rid != "" {
				out.ID = flexibleID(rid)
			}
		}

		_ = resp.Body.Close()
		return out, nil
	}

	return nil, fmt.Errorf("LinkedIn API %s %s -> exhausted %d retries after 429s", method, path, retryMax)
}

// parseRetryAfter returns how long to wait before retrying a 429, or 0 if no
// usable header is present. LinkedIn returns Retry-After either as a delay in
// seconds or as an HTTP-date; both forms are honored. Never returns a negative
// duration.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n > 0 {
			return time.Duration(n) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d
		}
	}
	return 0
}

// sleepCtx waits for d, returning early if ctx is cancelled.
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

// maxListPages bounds how many pages findByName will walk before giving up. It
// mirrors the Twitter client's cap. Since name-based idempotency depends on
// actually seeing a live same-name resource, hitting the cap with more pages
// still available is reported as an error rather than a silent no-match — a
// silent no-match would let the caller create a DUPLICATE.
const maxListPages = 25

// findByName searches a nested resource path for a live element matching name.
// It returns the trailing numeric ID, or "" when the resource genuinely does not
// exist (a 404 or an exhausted result set with no match). Statuses in
// skipStatuses are ignored. Mirrors findByName() (paginated search across all
// statuses).
//
// At LinkedIn-Version 202602 the search APIs use CURSOR pagination, not offset
// pagination: each response carries metadata.nextPageToken, and the client
// echoes that token back as the `pageToken` request param to fetch the next
// page. Pagination stops when nextPageToken comes back empty. An offset-based
// walk (start/count) or a "full page" heuristic is the wrong model for this API
// version and can miss results or loop, so it is not used.
//
// Unlike the TypeScript original, transient/unexpected search failures are NOT
// swallowed: a non-404 HTTP error, network error, or decode error is returned so
// the find-or-create caller aborts instead of proceeding to a create POST that
// would produce a duplicate. Pagination is followed to exhaustion (up to
// maxListPages); reaching the cap with a next-page token still present also
// returns an error rather than a false no-match.
func (c *Client) findByName(ctx context.Context, nestedPath, name string) (string, error) {
	return c.findMatch(ctx, nestedPath, func(el responseElement) bool {
		return el.Name == name
	})
}

// findCampaignByNameInGroup searches the campaign collection for a live campaign
// whose name matches AND whose parent campaignGroup URN resolves to groupID. The
// group constraint is essential: the campaign search is account-wide, so a
// same-name campaign under a DIFFERENT (e.g. archived/replaced) group would
// otherwise be returned as an idempotent match and the new campaign would never
// be created under the correct group. Elements missing a campaignGroup are not
// matched, since without it the parent cannot be confirmed.
func (c *Client) findCampaignByNameInGroup(ctx context.Context, campaignsPath, name, groupID string) (string, error) {
	return c.findMatch(ctx, campaignsPath, func(el responseElement) bool {
		return el.Name == name && el.CampaignGroup != "" && trailingID(el.CampaignGroup) == groupID
	})
}

// findMatch runs the cursor-paginated search-by name walk shared by findByName
// and findCampaignByNameInGroup, returning the trailing numeric ID of the first
// element for which match reports true (and whose status is not in
// skipStatuses), or "" when no such element exists. Error handling and the
// max-pages guard match the findByName contract documented above.
func (c *Client) findMatch(ctx context.Context, nestedPath string, match func(responseElement) bool) (string, error) {
	const pageSize = 50
	pageToken := ""
	for page := 0; page < maxListPages; page++ {
		params := map[string]string{
			"q": "search",
			// Cursor pagination at LinkedIn-Version 202602 uses `pageSize` (paired
			// with `pageToken`), NOT the legacy offset param `count`. Sending
			// `count` here was ignored by the cursor contract, so the page size the
			// caller asked for silently did not take effect. No offset param
			// (`start`/`count`) is sent — the cursor token alone advances pages.
			"pageSize": strconv.Itoa(pageSize),
		}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}
		resp, err := c.doRequest(ctx, http.MethodGet, nestedPath, nil, params)
		if err != nil {
			// A 404 means the collection/resource genuinely isn't there: treat as
			// a clean no-match. Any other error (401/429/5xx, network, decode) is
			// transient or unexpected and must propagate so we never create a
			// duplicate off a swallowed failure.
			var ae *apiError
			if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
				return "", nil
			}
			return "", fmt.Errorf("search %q by name: %w", nestedPath, err)
		}
		for _, el := range resp.Elements {
			if !match(el) {
				continue
			}
			if _, skip := skipStatuses[el.Status]; skip {
				continue
			}
			raw := el.ID.String()
			if raw == "" {
				raw = el.DURN
			}
			if raw == "" {
				raw = el.URN
			}
			if raw == "" {
				continue
			}
			return trailingID(raw), nil
		}
		// Cursor pagination: an empty nextPageToken marks the end of the result
		// set. Otherwise carry the token into the next request.
		if resp.Metadata.NextPageToken == "" {
			return "", nil
		}
		pageToken = resp.Metadata.NextPageToken
	}
	// Cap reached with a next-page token still present: refuse to report a false
	// no-match, which would let the caller create a duplicate resource.
	return "", fmt.Errorf("search %q by name: exceeded %d pages without exhausting results — aborting to avoid creating a duplicate", nestedPath, maxListPages)
}

// trailingID returns the segment after the last colon of a URN, or the input
// unchanged when it contains no colon. Mirrors `id.split(':').pop()`.
func trailingID(raw string) string {
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

// ---------------------------------------------------------------------------
// Timestamp helpers (milliseconds since epoch)
// ---------------------------------------------------------------------------

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// toMs converts a YYYY-MM-DD date to epoch milliseconds. Mirrors toMs():
//   - eod=false: start-of-day UTC; if in the past, returns now+5min.
//   - eod=true: end-of-day UTC (23:59:59.999); errors if in the past.
func (c *Client) toMs(dateStr string, eod bool) (int64, error) {
	if !dateRE.MatchString(dateStr) {
		return 0, fmt.Errorf("invalid date format: %s — expected YYYY-MM-DD", dateStr)
	}
	t, err := time.ParseInLocation("2006-01-02", dateStr, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("invalid date: %s", dateStr)
	}
	nowMs := c.now().UTC().UnixMilli()
	if eod {
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, int(999*time.Millisecond), time.UTC)
		endMs := end.UnixMilli()
		if endMs <= nowMs {
			return 0, fmt.Errorf("end date %s is in the past", dateStr)
		}
		return endMs, nil
	}
	startMs := t.UnixMilli()
	if startMs <= nowMs {
		return nowMs + 5*60*1000, nil
	}
	return startMs, nil
}

// ---------------------------------------------------------------------------
// Hierarchy creation
// ---------------------------------------------------------------------------

// findOrCreateCampaignGroup returns an existing ACTIVE-eligible group's ID or
// creates a new ACTIVE campaign group. Mirrors findOrCreateCampaignGroup():
// campaign groups are always created with status ACTIVE.
//
// Unexported by design: accountID is trusted to have already passed the
// cross-tenant fail-closed check (resolveAccountID) in CreateCampaign. Exposing
// it would let a caller create resources under an arbitrary, unvalidated account
// id, bypassing that check. All hierarchy helpers are internal for this reason.
func (c *Client) findOrCreateCampaignGroup(ctx context.Context, accountID, name, startDate, endDate string) (string, error) {
	groupsPath := fmt.Sprintf("adAccounts/%s/adCampaignGroups", accountID)

	existing, err := c.findByName(ctx, groupsPath, name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	startMs, err := c.toMs(startDate, false)
	if err != nil {
		return "", err
	}
	endMs, err := c.toMs(endDate, true)
	if err != nil {
		return "", err
	}
	if endMs <= startMs {
		return "", fmt.Errorf("end date (%s) must be after start date (%s)", endDate, startDate)
	}

	body := map[string]any{
		"account": accountURN(accountID),
		"name":    name,
		"status":  "ACTIVE",
		"runSchedule": map[string]any{
			"start": startMs,
			"end":   endMs,
		},
	}

	resp, err := c.doRequest(ctx, http.MethodPost, groupsPath, body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("LinkedIn API returned no ID for campaign group creation")
	}
	return trailingID(resp.ID.String()), nil
}

// createSponsoredCampaign returns an existing campaign's ID (idempotent by
// name) or creates a new PAUSED sponsored-updates campaign. Budget is sent as a
// decimal string (not micros); timestamps are milliseconds. Mirrors
// createCampaign().
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to
// have passed resolveAccountID in CreateCampaign.
func (c *Client) createSponsoredCampaign(ctx context.Context, accountID, groupID, name string, budgetUSD float64, geoURNs []string, targetingProfile, startDate, endDate string, lifetimeBudget bool) (string, error) {
	campaignsPath := fmt.Sprintf("adAccounts/%s/adCampaigns", accountID)

	// Scope the idempotency lookup to the resolved campaign group: the campaign
	// search is account-wide by name, so a same-name campaign under a DIFFERENT
	// (e.g. archived/replaced) group must NOT be treated as a match — otherwise a
	// new campaign is never created under the correct group.
	existing, err := c.findCampaignByNameInGroup(ctx, campaignsPath, name, groupID)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	startMs, err := c.toMs(startDate, false)
	if err != nil {
		return "", err
	}
	endMs, err := c.toMs(endDate, true)
	if err != nil {
		return "", err
	}
	if endMs <= startMs {
		return "", fmt.Errorf("end date (%s) must be after start date (%s)", endDate, startDate)
	}

	targeting, err := c.buildTargetingCriteria(targetingProfile, geoURNs)
	if err != nil {
		return "", err
	}

	// Budget as a decimal string, e.g. "100.00" — not micros. Mirrors toFixed(2).
	amount := strconv.FormatFloat(budgetUSD, 'f', 2, 64)
	budgetField := "dailyBudget"
	if lifetimeBudget {
		budgetField = "totalBudget"
	}

	body := map[string]any{
		"account":                accountURN(accountID),
		"campaignGroup":          "urn:li:sponsoredCampaignGroup:" + groupID,
		"name":                   name,
		"status":                 "PAUSED",
		"type":                   "SPONSORED_UPDATES",
		"objectiveType":          "WEBSITE_CONVERSION",
		"costType":               "CPM",
		"locale":                 map[string]any{"country": "US", "language": "en"},
		"offsiteDeliveryEnabled": true,
		"politicalIntent":        "NOT_POLITICAL",
		budgetField:              map[string]any{"amount": amount, "currencyCode": "USD"},
		"runSchedule":            map[string]any{"start": startMs, "end": endMs},
	}
	// Merge the targetingCriteria block.
	for k, v := range targeting {
		body[k] = v
	}

	resp, err := c.doRequest(ctx, http.MethodPost, campaignsPath, body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("LinkedIn API returned no ID for campaign creation")
	}
	return trailingID(resp.ID.String()), nil
}

// createDarkPost creates an unpublished-to-feed sponsored post
// (feedDistribution NONE) and returns its share URN. Mirrors createDarkPost().
//
// The post uses an article content block. Per the TS, callToAction is NOT sent
// for article ads. The dark-post nature comes from distribution.feedDistribution
// = "NONE".
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to
// have passed resolveAccountID in CreateCampaign.
func (c *Client) createDarkPost(ctx context.Context, accountID, introText, headline, destURL, imageURN string) (string, error) {
	author, err := c.orgURN(accountID)
	if err != nil {
		return "", err
	}

	intro := stripDashes(introText)
	// LinkedIn single-image ad intro/primary (commentary) text is capped at 600
	// characters; the TS source truncates intro_text too. Truncate rune-safely so
	// a multi-byte rune is never split into invalid UTF-8.
	if len([]rune(intro)) > 600 {
		intro = truncateRunes(intro, 600)
	}
	head := stripDashes(headline)
	if len([]rune(head)) > 200 {
		head = truncateRunes(head, 200)
	}

	article := map[string]any{
		"source":      destURL,
		"title":       head,
		"description": "",
	}
	if imageURN != "" {
		article["thumbnail"] = imageURN
	}

	body := map[string]any{
		"author":     author,
		"commentary": intro,
		"visibility": "PUBLIC",
		"distribution": map[string]any{
			"feedDistribution":               "NONE",
			"targetEntities":                 []any{},
			"thirdPartyDistributionChannels": []any{},
		},
		"content":        map[string]any{"article": article},
		"lifecycleState": "PUBLISHED",
		"adContext":      map[string]any{"dscAdAccount": accountURN(accountID)},
	}

	resp, err := c.doRequest(ctx, http.MethodPost, "posts", body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("LinkedIn API returned no ID for dark post creation")
	}
	return resp.ID.String(), nil
}

// createCreative creates a DRAFT creative referencing a share URN and returns
// its ID. Mirrors createCreative().
//
// Unexported by design (see findOrCreateCampaignGroup): accountID is trusted to
// have passed resolveAccountID in CreateCampaign.
func (c *Client) createCreative(ctx context.Context, accountID, campaignID, shareURN, adName string) (string, error) {
	body := map[string]any{
		"campaign":       "urn:li:sponsoredCampaign:" + campaignID,
		"intendedStatus": "DRAFT",
		"content":        map[string]any{"reference": shareURN},
	}
	if adName != "" {
		if len([]rune(adName)) > 255 {
			adName = truncateRunes(adName, 255)
		}
		body["name"] = adName
	}

	resp, err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("adAccounts/%s/creatives", accountID), body, nil)
	if err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("LinkedIn API returned no ID for creative creation")
	}
	return resp.ID.String(), nil
}

// ---------------------------------------------------------------------------
// UTM helper
// ---------------------------------------------------------------------------

// BuildUTMURL appends LinkedIn UTM params to baseURL for a given variant.
// Mirrors buildLinkedInUtmUrl().
//
// The URL is parsed so UTM params merge into the query and the fragment stays at
// the end: naive string concatenation on "https://x.org/reg#tickets" would yield
// "https://x.org/reg#tickets?utm_..." (query inside the fragment, which browsers
// drop). Any existing query params are preserved.
func BuildUTMURL(baseURL, hsToken, campaignName string, variantIndex int) string {
	term := strings.ReplaceAll(campaignName, " | ", "_")
	term = strings.Join(strings.Fields(term), "-")
	term = strings.ToLower(term)

	u, err := url.Parse(baseURL)
	if err != nil {
		// Fall back to concatenation if the URL is unparseable; better a slightly
		// malformed URL than dropping the UTM params entirely.
		trimmed := strings.TrimRight(baseURL, "/")
		sep := "?"
		if strings.Contains(baseURL, "?") {
			sep = "&"
		}
		return trimmed + sep + utmValues(hsToken, term, variantIndex).Encode()
	}

	q := u.Query()
	for k, vals := range utmValues(hsToken, term, variantIndex) {
		for _, val := range vals {
			q.Set(k, val)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// utmValues builds the LinkedIn UTM query parameters.
func utmValues(hsToken, term string, variantIndex int) url.Values {
	v := url.Values{}
	v.Set("utm_source", "linkedin")
	v.Set("utm_medium", "paid-social")
	if hsToken != "" {
		v.Set("utm_campaign", hsToken)
	}
	v.Set("utm_term", term)
	v.Set("utm_content", fmt.Sprintf("variant-%d", variantIndex))
	return v
}

// truncateRunes returns at most n runes of s, never splitting a multi-byte rune
// (byte-slicing would corrupt non-ASCII text into invalid UTF-8 that json.Marshal
// replaces with U+FFFD). Mirrors the TS substring behavior.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// stripDashes normalizes em/en dashes to commas. Mirrors the TS stripDashes.
func stripDashes(text string) string {
	// " — "/" – " (with surrounding spaces) -> ", "
	text = strings.ReplaceAll(text, " — ", ", ")
	text = strings.ReplaceAll(text, " – ", ", ")
	// bare em/en dashes -> ", "
	text = strings.ReplaceAll(text, "—", ", ")
	text = strings.ReplaceAll(text, "–", ", ")
	// trim a leading or trailing ", "
	text = strings.TrimPrefix(text, ", ")
	text = strings.TrimSuffix(text, ", ")
	return text
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

// validatePrerequisites probes runtime-config-dependent lookups before any
// side-effecting call, so a config gap can't leave orphan LinkedIn artifacts.
// Mirrors validateLinkedInPrerequisites().
func (c *Client) validatePrerequisites(accountID, profile string) error {
	if _, err := c.orgURN(accountID); err != nil {
		return err
	}
	lookup := profile
	if profile == "custom" {
		lookup = "cloud-native"
	}
	for _, p := range c.cfg.TargetingProfiles {
		if p.ID == lookup {
			return nil
		}
	}
	if profile == "custom" {
		// custom tolerates a missing cloud-native fallback (empty targeting).
		return nil
	}
	return fmt.Errorf("LinkedIn targeting profile %q not found in runtime config — refusing to start campaign creation", profile)
}

// validateRegistrationURL rejects a registration URL before any permanent
// resource is created. LinkedIn's ad API only surfaces a bad landing-page URL
// AFTER the campaign group and campaign already exist, orphaning them; catching
// it up front keeps CreateCampaign side-effect-free on invalid input. The URL
// must parse, be absolute, and use an http/https scheme.
func validateRegistrationURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("registration URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("registration URL %q is not a valid URL: %w", raw, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("registration URL %q must be absolute (include scheme and host)", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", raw, u.Scheme)
	}
}

// CreateCampaign runs the full campaign-creation flow: verify prerequisites,
// find/create the ACTIVE campaign group, create the PAUSED campaign, then for
// each variant create a dark post and a DRAFT creative. Mirrors
// executeLinkedInCampaignCreation().
//
// Resumability limitation: variant creation is NOT idempotent. If a later
// variant fails after earlier ones succeeded, the group and campaign are found
// (idempotent by name) on a retry, but each already-created dark post is
// recreated because dark posts have no name-based lookup — a blind retry would
// duplicate the surviving creatives. To keep the caller from retrying blindly,
// a mid-variant failure returns an error that states how many variants
// succeeded versus failed AND still returns a *CampaignResult carrying the
// group/campaign IDs and the steps completed so far (including the created
// creatives). Callers should inspect the partial result rather than re-invoking
// CreateCampaign unchanged. A full idempotent-resume implementation is out of
// scope for this fix.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	accountID, err := c.resolveAccountID(in.AdAccountID)
	if err != nil {
		return nil, err
	}

	if err := c.validatePrerequisites(accountID, in.TargetingProfile); err != nil {
		return nil, err
	}

	// EventName is semantically required: it is the sole distinguishing token in
	// both the campaign-group name ("Events | <EventName> | <Project>") and the
	// campaign name, so an empty or whitespace-only value collapses every campaign
	// to the same idempotency key (e.g. "Events |  | TLF"). Reject it up front,
	// before any POST that would create a permanent, mislabeled resource.
	//
	// Trim ONCE here and use the trimmed value everywhere downstream (group name,
	// campaign name, ad name / idempotency keys): validating a trimmed value but
	// then building resources from the original untrimmed field let a value like
	// "  KubeCon  " pass validation yet produce resources with leading/trailing
	// whitespace and an inconsistent idempotency key.
	eventName := strings.TrimSpace(in.EventName)
	if eventName == "" {
		return nil, fmt.Errorf("event name is required and must not be empty or whitespace-only")
	}

	// Trim the registration URL ONCE up front and use the trimmed value both for
	// validation and everywhere downstream (BuildUTMURL). Validating a trimmed
	// value but then building the UTM URL from the original untrimmed field let a
	// value with surrounding whitespace pass validation yet produce a malformed
	// UTM URL (embedded spaces).
	reg := strings.TrimSpace(in.RegistrationURL)

	// Validate the registration URL BEFORE any POST so an empty/relative/malformed
	// URL is rejected up front rather than after the campaign group and campaign
	// (permanent resources) already exist.
	if err := validateRegistrationURL(reg); err != nil {
		return nil, err
	}

	// Validate the budget BEFORE any POST. BudgetUSD is formatted straight into
	// the campaign body, so a non-positive, NaN, or Inf value would otherwise be
	// rejected by LinkedIn only AFTER the campaign group (a permanent resource)
	// already exists, orphaning it.
	if math.IsNaN(in.BudgetUSD) || math.IsInf(in.BudgetUSD, 0) {
		return nil, fmt.Errorf("budget must be a finite number, got %v", in.BudgetUSD)
	}
	if in.BudgetUSD <= 0 {
		return nil, fmt.Errorf("budget must be greater than zero, got %v", in.BudgetUSD)
	}

	// Refuse to create a campaign with no creatives: LinkedIn campaign-group and
	// campaign creation are permanent side effects, so an empty variant set would
	// leave an orphaned, un-adorned campaign upstream.
	if len(in.Variants) == 0 {
		return nil, fmt.Errorf("at least one creative variant is required")
	}

	// Trim Project ONCE and use the trimmed value everywhere. Checking only the
	// exact empty string let a whitespace-only Project like "   " slip past the
	// default and be embedded verbatim in the group/campaign names; a padded
	// project like "  cncf  " would likewise carry its whitespace into resource
	// names. Default to "TLF" when empty after trimming.
	project := strings.TrimSpace(in.Project)
	if project == "" {
		project = "TLF"
	}

	steps := []string{}

	// Build the geo URN set and refuse to create anything with no usable geo
	// targeting BEFORE the first create POST. ResolveGeoTargets deliberately drops
	// unknown geos, so a caller passing only unknown geos arrives here with an
	// empty URN set. Creating the campaign group/campaign anyway (both permanent
	// side effects) would leave an orphaned campaign with empty geo targeting.
	geoURNs := make([]string, 0, len(in.GeoTargets))
	for _, g := range in.GeoTargets {
		if g.URN == "" {
			continue
		}
		geoURNs = append(geoURNs, g.URN)
	}
	if len(geoURNs) == 0 {
		return nil, fmt.Errorf("no usable geo targets: all supplied geos resolved to nothing — refusing to create a campaign with empty geo targeting")
	}

	groupName := fmt.Sprintf("Events | %s | %s", eventName, project)
	groupID, err := c.findOrCreateCampaignGroup(ctx, accountID, groupName, in.StartDate, in.EndDate)
	if err != nil {
		return nil, err
	}
	steps = append(steps, fmt.Sprintf("Campaign group: %s (ID: %s)", groupName, groupID))

	campaignName := fmt.Sprintf("Events | %s | LinkedIn | Conversions | Prospecting | Static | %s | MoFU", eventName, project)
	campaignID, err := c.createSponsoredCampaign(ctx, accountID, groupID, campaignName, in.BudgetUSD, geoURNs, in.TargetingProfile, in.StartDate, in.EndDate, in.LifetimeBudget)
	if err != nil {
		return nil, err
	}
	steps = append(steps, fmt.Sprintf("Campaign created (PAUSED): %s (ID: %s)", campaignName, campaignID))

	creativeCount := 0
	for i, variant := range in.Variants {
		destURL := BuildUTMURL(reg, in.HSToken, campaignName, i+1)
		shareURN, err := c.createDarkPost(ctx, accountID, variant.IntroText, variant.Headline, destURL, variant.ImageURN)
		if err != nil {
			return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps),
				fmt.Errorf("variant-%d dark post failed after %d of %d variant(s) created: %w — group %q and campaign %q already exist; do NOT blindly retry (would duplicate the %d created creative(s)); inspect the returned partial result", i+1, creativeCount, len(in.Variants), err, groupID, campaignID, creativeCount)
		}
		steps = append(steps, fmt.Sprintf("Dark post variant-%d: %s", i+1, shareURN))

		adName := fmt.Sprintf("%s | variant-%d", eventName, i+1)
		creativeID, err := c.createCreative(ctx, accountID, campaignID, shareURN, adName)
		if err != nil {
			return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps),
				fmt.Errorf("variant-%d creative failed after %d of %d variant(s) created: %w — group %q and campaign %q already exist; do NOT blindly retry (would duplicate the %d created creative(s)); inspect the returned partial result", i+1, creativeCount, len(in.Variants), err, groupID, campaignID, creativeCount)
		}
		steps = append(steps, fmt.Sprintf("Creative (DRAFT): %s", creativeID))
		creativeCount++
	}

	return c.buildResult(accountID, groupName, groupID, campaignName, campaignID, creativeCount, steps), nil
}

// buildResult assembles a CampaignResult from the created hierarchy pieces.
func (c *Client) buildResult(accountID, groupName, groupID, campaignName, campaignID string, creativeCount int, steps []string) *CampaignResult {
	return &CampaignResult{
		Platform:          "linkedin-ads",
		CampaignGroupName: groupName,
		CampaignGroupID:   groupID,
		CampaignName:      campaignName,
		CampaignID:        campaignID,
		CreativeCount:     creativeCount,
		LinkedInURL:       fmt.Sprintf("https://www.linkedin.com/campaignmanager/accounts/%s/campaigns/%s", accountID, campaignID),
		Steps:             steps,
	}
}
