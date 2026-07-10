// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Elements []responseElement `json:"elements"`
}

// responseElement mirrors LinkedInResponseElement. LinkedIn returns an
// element's identifier under any of `id`, `$URN`, or `urn` depending on the
// endpoint, so each is decoded into its own field and the read sites fall back
// through ID → DURN → URN.
type responseElement struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	ID     string `json:"id"`
	URN    string `json:"urn"`
	DURN   string `json:"$URN"`
}

var pathValidRE = regexp.MustCompile(`^[a-zA-Z0-9/_:?=&.-]*$`)

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
			return nil, fmt.Errorf("LinkedIn API %s %s -> %d: %s", method, path, resp.StatusCode, text)
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
				out.ID = rid
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

// findByName searches a nested resource path for a live element matching name.
// It returns the trailing numeric ID, or "" if none. Statuses in skipStatuses
// are ignored. Mirrors findByName() (paginated search across all statuses).
// Search failures are swallowed and reported as no-match, matching the TS.
func (c *Client) findByName(ctx context.Context, nestedPath, name string) string {
	const pageSize = 50
	const maxPages = 5
	start := 0
	for page := 0; page < maxPages; page++ {
		resp, err := c.doRequest(ctx, http.MethodGet, nestedPath, nil, map[string]string{
			"q":     "search",
			"count": strconv.Itoa(pageSize),
			"start": strconv.Itoa(start),
		})
		if err != nil {
			return ""
		}
		for _, el := range resp.Elements {
			if el.Name != name {
				continue
			}
			if _, skip := skipStatuses[el.Status]; skip {
				continue
			}
			raw := el.ID
			if raw == "" {
				raw = el.DURN
			}
			if raw == "" {
				raw = el.URN
			}
			if raw == "" {
				continue
			}
			return trailingID(raw)
		}
		if len(resp.Elements) < pageSize {
			break
		}
		start += pageSize
	}
	return ""
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

// FindOrCreateCampaignGroup returns an existing ACTIVE-eligible group's ID or
// creates a new ACTIVE campaign group. Mirrors findOrCreateCampaignGroup():
// campaign groups are always created with status ACTIVE.
func (c *Client) FindOrCreateCampaignGroup(ctx context.Context, accountID, name, startDate, endDate string) (string, error) {
	groupsPath := fmt.Sprintf("adAccounts/%s/adCampaignGroups", accountID)

	if existing := c.findByName(ctx, groupsPath, name); existing != "" {
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
	return trailingID(resp.ID), nil
}

// CreateSponsoredCampaign returns an existing campaign's ID (idempotent by
// name) or creates a new PAUSED sponsored-updates campaign. Budget is sent as a
// decimal string (not micros); timestamps are milliseconds. Mirrors
// createCampaign().
func (c *Client) CreateSponsoredCampaign(ctx context.Context, accountID, groupID, name string, budgetUSD float64, geoURNs []string, targetingProfile, startDate, endDate string, lifetimeBudget bool) (string, error) {
	campaignsPath := fmt.Sprintf("adAccounts/%s/adCampaigns", accountID)

	if existing := c.findByName(ctx, campaignsPath, name); existing != "" {
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
	return trailingID(resp.ID), nil
}

// CreateDarkPost creates an unpublished-to-feed sponsored post
// (feedDistribution NONE) and returns its share URN. Mirrors createDarkPost().
//
// The post uses an article content block. Per the TS, callToAction is NOT sent
// for article ads. The dark-post nature comes from distribution.feedDistribution
// = "NONE".
func (c *Client) CreateDarkPost(ctx context.Context, accountID, introText, headline, destURL, imageURN string) (string, error) {
	author, err := c.orgURN(accountID)
	if err != nil {
		return "", err
	}

	intro := stripDashes(introText)
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
	return resp.ID, nil
}

// CreateCreative creates a DRAFT creative referencing a share URN and returns
// its ID. Mirrors createCreative().
func (c *Client) CreateCreative(ctx context.Context, accountID, campaignID, shareURN, adName string) (string, error) {
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
	return resp.ID, nil
}

// ---------------------------------------------------------------------------
// UTM helper
// ---------------------------------------------------------------------------

// BuildUTMURL appends LinkedIn UTM params to baseURL for a given variant.
// Mirrors buildLinkedInUtmUrl().
func BuildUTMURL(baseURL, hsToken, campaignName string, variantIndex int) string {
	term := strings.ReplaceAll(campaignName, " | ", "_")
	term = strings.Join(strings.Fields(term), "-")
	term = strings.ToLower(term)

	v := url.Values{}
	v.Set("utm_source", "linkedin")
	v.Set("utm_medium", "paid-social")
	if hsToken != "" {
		v.Set("utm_campaign", hsToken)
	}
	v.Set("utm_term", term)
	v.Set("utm_content", fmt.Sprintf("variant-%d", variantIndex))

	trimmed := strings.TrimRight(baseURL, "/")
	sep := "?"
	if strings.Contains(baseURL, "?") {
		sep = "&"
	}
	return trimmed + sep + v.Encode()
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

// CreateCampaign runs the full campaign-creation flow: verify prerequisites,
// find/create the ACTIVE campaign group, create the PAUSED campaign, then for
// each variant create a dark post and a DRAFT creative. Mirrors
// executeLinkedInCampaignCreation().
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	accountID, err := c.resolveAccountID(in.AdAccountID)
	if err != nil {
		return nil, err
	}

	if err := c.validatePrerequisites(accountID, in.TargetingProfile); err != nil {
		return nil, err
	}

	// Refuse to create a campaign with no creatives: LinkedIn campaign-group and
	// campaign creation are permanent side effects, so an empty variant set would
	// leave an orphaned, un-adorned campaign upstream.
	if len(in.Variants) == 0 {
		return nil, fmt.Errorf("at least one creative variant is required")
	}

	project := in.Project
	if project == "" {
		project = "TLF"
	}

	steps := []string{}

	groupName := fmt.Sprintf("Events | %s | %s", in.EventName, project)
	groupID, err := c.FindOrCreateCampaignGroup(ctx, accountID, groupName, in.StartDate, in.EndDate)
	if err != nil {
		return nil, err
	}
	steps = append(steps, fmt.Sprintf("Campaign group: %s (ID: %s)", groupName, groupID))

	geoURNs := make([]string, 0, len(in.GeoTargets))
	for _, g := range in.GeoTargets {
		geoURNs = append(geoURNs, g.URN)
	}

	campaignName := fmt.Sprintf("Events | %s | LinkedIn | Conversions | Prospecting | Static | %s | MoFU", in.EventName, project)
	campaignID, err := c.CreateSponsoredCampaign(ctx, accountID, groupID, campaignName, in.BudgetUSD, geoURNs, in.TargetingProfile, in.StartDate, in.EndDate, in.LifetimeBudget)
	if err != nil {
		return nil, err
	}
	steps = append(steps, fmt.Sprintf("Campaign created (PAUSED): %s (ID: %s)", campaignName, campaignID))

	creativeCount := 0
	for i, variant := range in.Variants {
		destURL := BuildUTMURL(in.RegistrationURL, in.HSToken, campaignName, i+1)
		shareURN, err := c.CreateDarkPost(ctx, accountID, variant.IntroText, variant.Headline, destURL, variant.ImageURN)
		if err != nil {
			return nil, err
		}
		steps = append(steps, fmt.Sprintf("Dark post variant-%d: %s", i+1, shareURN))

		adName := fmt.Sprintf("%s | variant-%d", in.EventName, i+1)
		creativeID, err := c.CreateCreative(ctx, accountID, campaignID, shareURN, adName)
		if err != nil {
			return nil, err
		}
		steps = append(steps, fmt.Sprintf("Creative (DRAFT): %s", creativeID))
		creativeCount++
	}

	return &CampaignResult{
		Platform:          "linkedin-ads",
		CampaignGroupName: groupName,
		CampaignGroupID:   groupID,
		CampaignName:      campaignName,
		CampaignID:        campaignID,
		CreativeCount:     creativeCount,
		LinkedInURL:       fmt.Sprintf("https://www.linkedin.com/campaignmanager/accounts/%s/campaigns/%s", accountID, campaignID),
		Steps:             steps,
	}, nil
}
