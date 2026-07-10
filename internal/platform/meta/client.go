// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package meta implements a Go client for the Meta (Facebook/Instagram) Ads
// platform, ported from the upstream TypeScript meta-ads.service.ts.
//
// The client speaks to the Meta Graph API using a Bearer access token and
// creates a Campaign -> Ad Set -> Ad(s) hierarchy. Credentials and account
// configuration are injected via NewClient; nothing in this package reads the
// process environment.
package meta

import (
	"bytes"
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------------------
// Constants (mirrored from meta.constants.ts and @lfx-one/shared/constants)
// ---------------------------------------------------------------------------

const (
	// DefaultBaseURL is the Meta Graph API base URL (mirrors META_BASE_URL).
	DefaultBaseURL = "https://graph.facebook.com/v25.0"
	// DefaultAdsManagerURL is the Meta Ads Manager base URL (mirrors META_ADS_MANAGER_URL).
	DefaultAdsManagerURL = "https://adsmanager.facebook.com"
	// DefaultRequestTimeout mirrors META_REQUEST_TIMEOUT_MS (30s).
	DefaultRequestTimeout = 30 * time.Second

	// retryMax is the number of times a 429 (rate-limited) request is retried
	// before giving up. Mirrors the resilience the Twitter client applies.
	retryMax = 3
	// retryBaseDelay is the base for exponential backoff when the API returns a
	// 429 without a usable Retry-After header (1s, 2s, 4s, ...).
	retryBaseDelay = 1 * time.Second
	// maxRetryWait caps how long a single 429 backoff waits, so an outsized
	// Retry-After value can't stall a request past the point of usefulness.
	maxRetryWait = 60 * time.Second
	// drainLimit bounds how much of a 429 response body is drained before close
	// so the connection can be reused, without reading an unbounded body.
	drainLimit = 64 << 10
)

// ---------------------------------------------------------------------------
// Objective -> parameter mapping (mirrors META_OBJECTIVE_PARAMS)
// ---------------------------------------------------------------------------

// PromotedObjectType identifies which promoted_object shape an objective needs.
type PromotedObjectType string

const (
	// PromotedObjectNone means the objective needs no promoted_object.
	PromotedObjectNone PromotedObjectType = ""
	// PromotedObjectPageID means the promoted_object carries a page_id.
	PromotedObjectPageID PromotedObjectType = "page_id"
	// PromotedObjectPixelID means the promoted_object carries a pixel_id.
	PromotedObjectPixelID PromotedObjectType = "pixel_id"
)

// ObjectiveParams describes the Meta API parameters for a marketing objective.
type ObjectiveParams struct {
	CampaignObjective  string
	OptimizationGoal   string
	PromotedObjectType PromotedObjectType
}

// objectiveParams maps the user-facing objective to Meta Graph API v25.0
// ODAX outcome objectives, optimization goals, and promoted-object needs.
// Mirrors META_OBJECTIVE_PARAMS from @lfx-one/shared/constants.
var objectiveParams = map[string]ObjectiveParams{
	"awareness": {
		CampaignObjective:  "OUTCOME_AWARENESS",
		OptimizationGoal:   "REACH",
		PromotedObjectType: PromotedObjectNone,
	},
	"traffic": {
		CampaignObjective:  "OUTCOME_TRAFFIC",
		OptimizationGoal:   "LINK_CLICKS",
		PromotedObjectType: PromotedObjectNone,
	},
	"engagement": {
		CampaignObjective:  "OUTCOME_ENGAGEMENT",
		OptimizationGoal:   "POST_ENGAGEMENT",
		PromotedObjectType: PromotedObjectPageID,
	},
	"leads": {
		CampaignObjective:  "OUTCOME_LEADS",
		OptimizationGoal:   "LEAD_GENERATION",
		PromotedObjectType: PromotedObjectPageID,
	},
	"conversions": {
		CampaignObjective:  "OUTCOME_SALES",
		OptimizationGoal:   "OFFSITE_CONVERSIONS",
		PromotedObjectType: PromotedObjectPixelID,
	},
}

// objectiveLabels mirrors OBJECTIVE_LABELS.
var objectiveLabels = map[string]string{
	"awareness":   "Awareness",
	"traffic":     "Traffic",
	"engagement":  "Engagement",
	"leads":       "Leads",
	"conversions": "Conversions",
}

// ObjectiveParamsFor returns the Meta parameters for the given objective and
// whether the objective is known. Exposed to support mapping-correctness tests.
func ObjectiveParamsFor(objective string) (ObjectiveParams, bool) {
	p, ok := objectiveParams[objective]
	return p, ok
}

// ---------------------------------------------------------------------------
// Placements (mirrors MetaPlacement + META_DEFAULT_PLACEMENTS)
// ---------------------------------------------------------------------------

// Placement toggles the ad placements requested for an ad set. Each field is a
// pointer so callers can leave a placement unset and fall back to the default.
type Placement struct {
	FacebookFeed    *bool
	InstagramFeed   *bool
	Stories         *bool
	Reels           *bool
	AudienceNetwork *bool
	MessengerInbox  *bool
}

// defaultPlacements mirrors META_DEFAULT_PLACEMENTS: feed placements on,
// stories/reels/audience-network/messenger off.
var defaultPlacements = Placement{
	FacebookFeed:    boolPtr(true),
	InstagramFeed:   boolPtr(true),
	Stories:         boolPtr(false),
	Reels:           boolPtr(false),
	AudienceNetwork: boolPtr(false),
	MessengerInbox:  boolPtr(false),
}

func boolPtr(b bool) *bool { return &b }

// mergePlacements applies caller overrides on top of the defaults, matching
// the TS spread `{ ...META_DEFAULT_PLACEMENTS, ...placements }`.
func mergePlacements(over Placement) Placement {
	out := defaultPlacements
	if over.FacebookFeed != nil {
		out.FacebookFeed = over.FacebookFeed
	}
	if over.InstagramFeed != nil {
		out.InstagramFeed = over.InstagramFeed
	}
	if over.Stories != nil {
		out.Stories = over.Stories
	}
	if over.Reels != nil {
		out.Reels = over.Reels
	}
	if over.AudienceNetwork != nil {
		out.AudienceNetwork = over.AudienceNetwork
	}
	if over.MessengerInbox != nil {
		out.MessengerInbox = over.MessengerInbox
	}
	return out
}

func deref(b *bool) bool { return b != nil && *b }

// ---------------------------------------------------------------------------
// Credentials, account config, and client
// ---------------------------------------------------------------------------

// Credentials holds the Meta Graph API Bearer access token. Injected, never
// read from the environment.
type Credentials struct {
	AccessToken string
}

// AccountConfig identifies the Meta ad account and Facebook Page to operate on.
type AccountConfig struct {
	// AccountID is the ad account id, e.g. "act_193556282970417".
	AccountID string
	// PageID is the Facebook Page id used for creatives and promoted objects.
	PageID string
	// Label is an optional human-readable account label.
	Label string
}

// Client is a Meta Ads Graph API client.
type Client struct {
	creds         Credentials
	account       AccountConfig
	httpClient    *http.Client
	baseURL       string
	adsManagerURL string
	// timeNow allows tests to control the clock used for 429 backoff.
	// Defaults to time.Now.
	timeNow func() time.Time
	// retryBaseDelay is the base for exponential 429 backoff. Defaults to the
	// retryBaseDelay const; tests may shrink it to keep runs fast.
	retryBaseDelay time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client (useful for tests / timeouts).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithBaseURL overrides the Graph API base URL (useful for tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithAdsManagerURL overrides the Ads Manager base URL.
func WithAdsManagerURL(u string) Option {
	return func(c *Client) { c.adsManagerURL = strings.TrimRight(u, "/") }
}

// WithClock overrides the time source used for 429 backoff. For tests.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.timeNow = now
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

// NewClient constructs a Client from injected credentials and account config.
func NewClient(creds Credentials, account AccountConfig, opts ...Option) *Client {
	c := &Client{
		creds:          creds,
		account:        account,
		httpClient:     &http.Client{Timeout: DefaultRequestTimeout},
		baseURL:        DefaultBaseURL,
		adsManagerURL:  DefaultAdsManagerURL,
		timeNow:        time.Now,
		retryBaseDelay: retryBaseDelay,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---------------------------------------------------------------------------
// HTTP helper (mirrors metaRequest)
// ---------------------------------------------------------------------------

// createResponse mirrors the TS MetaCreateResponse: every create call returns
// at least an id field.
type createResponse struct {
	ID string `json:"id"`
}

// graphErrorEnvelope models the Graph API error body: {"error": {...}}.
type graphErrorEnvelope struct {
	Error *graphError `json:"error"`
}

type graphError struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      int    `json:"code"`
	FBTraceID string `json:"fbtrace_id"`
}

// APIError is returned when the Meta API responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	// Message is the Graph API error message when present, else the raw body.
	Message string
}

func (e *APIError) Error() string {
	// Mirror the TS behavior of not leaking full bodies to callers while still
	// surfacing status; include the parsed message when available.
	if e.Message != "" {
		return fmt.Sprintf("meta API request failed (%d): %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("meta API request failed (%d) with no error details in the response body", e.StatusCode)
}

// doRequest performs a Graph API call and decodes the JSON body into out.
// It honors ctx via http.NewRequestWithContext. A 429 (rate-limited) response is
// retried up to retryMax times with a bounded backoff (honoring Retry-After when
// present), since CreateCampaign issues several sequential Graph API calls that
// can trip Meta's per-app/account rate limits mid-flow.
func (c *Client) doRequest(ctx context.Context, method, path string, body map[string]any, out any) error {
	if c.creds.AccessToken == "" {
		return fmt.Errorf("meta access token is not configured")
	}

	var encoded []byte
	if body != nil && method == http.MethodPost {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
	}

	for attempt := 0; attempt <= retryMax; attempt++ {
		var reqBody io.Reader
		if encoded != nil {
			reqBody = bytes.NewReader(encoded)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("meta API %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < retryMax {
			wait := c.parseRetryAfter(resp)
			// Drain (bounded) before closing so the HTTP transport can reuse this
			// connection for the retry instead of opening a fresh TCP/TLS conn while
			// already rate-limited.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
			_ = resp.Body.Close()
			if wait <= 0 {
				wait = c.retryBaseDelay * time.Duration(1<<uint(attempt))
			}
			if wait > maxRetryWait {
				wait = maxRetryWait
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		}

		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := &APIError{StatusCode: resp.StatusCode, Method: method, Path: path}
			var env graphErrorEnvelope
			if json.Unmarshal(raw, &env) == nil && env.Error != nil && env.Error.Message != "" {
				apiErr.Message = env.Error.Message
			} else if snippet := strings.TrimSpace(string(raw)); snippet != "" {
				// Non-Graph or malformed error body: surface a truncated snippet of
				// the raw body so the real reason isn't lost.
				apiErr.Message = truncate(snippet, 300)
			}
			return apiErr
		}

		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
		}
		return nil
	}

	return &APIError{StatusCode: http.StatusTooManyRequests, Method: method, Path: path,
		Message: fmt.Sprintf("exhausted %d retries after 429s", retryMax)}
}

// parseRetryAfter returns how long to wait before retrying a 429, or 0 if no
// usable header is present. Meta returns Retry-After either as a delay in seconds
// or as an HTTP-date; both forms are honored. Never returns a negative duration.
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
		if d := t.Sub(c.timeNow()); d > 0 {
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

// ---------------------------------------------------------------------------
// Validation helpers (mirror the TS helpers)
// ---------------------------------------------------------------------------

var geoCodeRE = regexp.MustCompile(`^[A-Z]{2}$`)

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func validateRegistrationURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("registration URL is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("registration URL must use HTTPS")
	}
	return nil
}

// validateGeoTargets uppercases, trims, and filters to ISO-2 codes; defaults to
// ["US"] when nothing valid remains (mirrors validateGeoTargets).
func validateGeoTargets(geoTargets []string) []string {
	valid := make([]string, 0, len(geoTargets))
	for _, g := range geoTargets {
		up := strings.ToUpper(strings.TrimSpace(g))
		if geoCodeRE.MatchString(up) {
			valid = append(valid, up)
		}
	}
	if len(valid) == 0 {
		return []string{"US"}
	}
	return valid
}

// regulatedCountries require a Universal Ads Declaration / regional compliance
// and are excluded from API targeting (mirrors REGULATED_COUNTRIES).
var regulatedCountries = map[string]bool{"SG": true, "TW": true, "KR": true}

// geoToRegion mirrors GEO_TO_REGION.
var geoToRegion = map[string]string{
	"US": "NA", "CA": "NA", "MX": "NA",
	"GB": "EMEA", "DE": "EMEA", "FR": "EMEA", "NL": "EMEA", "SE": "EMEA",
	"CH": "EMEA", "ES": "EMEA", "IT": "EMEA", "AT": "EMEA", "BE": "EMEA", "IL": "EMEA",
	"IN": "India",
	"JP": "APAC", "KR": "APAC", "SG": "APAC", "AU": "APAC", "CN": "APAC", "TW": "APAC", "HK": "APAC",
	"BR": "LATAM",
}

func resolveRegion(geoTargets []string) string {
	if len(geoTargets) == 0 {
		return "Global"
	}
	primary := strings.ToUpper(geoTargets[0])
	if r, ok := geoToRegion[primary]; ok {
		return r
	}
	return "Global"
}

// ---------------------------------------------------------------------------
// Objective / placement / name / UTM builders
// ---------------------------------------------------------------------------

func buildPromotedObject(objective, pageID, pixelID string) (map[string]any, error) {
	params := objectiveParams[objective]
	switch params.PromotedObjectType {
	case PromotedObjectPageID:
		return map[string]any{"page_id": pageID}, nil
	case PromotedObjectPixelID:
		trimmed := strings.TrimSpace(pixelID)
		if trimmed == "" {
			return nil, fmt.Errorf("pixelID must be a non-empty string for '%s' objective", objective)
		}
		return map[string]any{"pixel_id": trimmed, "custom_event_type": "PURCHASE"}, nil
	default:
		return nil, nil
	}
}

func buildPlacementTargeting(over Placement) (map[string]any, error) {
	pl := mergePlacements(over)

	var publisherPlatforms, facebookPositions, instagramPositions, messengerPositions []string
	hasPlatform := func(p string) bool {
		for _, x := range publisherPlatforms {
			if x == p {
				return true
			}
		}
		return false
	}
	addPlatform := func(p string) {
		if !hasPlatform(p) {
			publisherPlatforms = append(publisherPlatforms, p)
		}
	}

	if deref(pl.FacebookFeed) {
		addPlatform("facebook")
		facebookPositions = append(facebookPositions, "feed")
	}
	if deref(pl.InstagramFeed) {
		addPlatform("instagram")
		instagramPositions = append(instagramPositions, "stream")
	}
	if deref(pl.Stories) {
		addPlatform("facebook")
		addPlatform("instagram")
		facebookPositions = append(facebookPositions, "story")
		instagramPositions = append(instagramPositions, "story")
	}
	if deref(pl.Reels) {
		addPlatform("facebook")
		addPlatform("instagram")
		facebookPositions = append(facebookPositions, "facebook_reels")
		instagramPositions = append(instagramPositions, "reels")
	}
	if deref(pl.AudienceNetwork) {
		publisherPlatforms = append(publisherPlatforms, "audience_network")
	}
	if deref(pl.MessengerInbox) {
		publisherPlatforms = append(publisherPlatforms, "messenger")
		messengerPositions = append(messengerPositions, "messenger_home")
	}

	if len(publisherPlatforms) == 0 {
		return nil, fmt.Errorf("at least one placement must be enabled (facebookFeed, instagramFeed, stories, reels, audienceNetwork, or messengerInbox)")
	}

	targeting := map[string]any{"publisher_platforms": publisherPlatforms}
	if len(facebookPositions) > 0 {
		targeting["facebook_positions"] = facebookPositions
	}
	if len(instagramPositions) > 0 {
		targeting["instagram_positions"] = instagramPositions
	}
	if len(messengerPositions) > 0 {
		targeting["messenger_positions"] = messengerPositions
	}
	return targeting, nil
}

func objectiveLabel(objective string) string {
	if l, ok := objectiveLabels[objective]; ok {
		return l
	}
	return objective
}

// buildCampaignName mirrors buildMetaCampaignName using the (already
// geo-filtered) targets to resolve the region segment.
func buildCampaignName(in CampaignInput, geoTargets []string) string {
	event := strings.ReplaceAll(in.EventName, "|", "-")
	region := resolveRegion(geoTargets)
	objective := objectiveLabel(defaultObjective(in.Objective))
	project := in.Project
	if strings.TrimSpace(project) == "" {
		project = "Linux Foundation"
	}
	project = strings.ReplaceAll(project, "|", "-")
	return fmt.Sprintf("Events | %s | %s | %s | Intent | Social | %s | MoFU", event, region, objective, project)
}

// buildUTMURL mirrors buildMetaUtmUrl.
func buildUTMURL(in CampaignInput, variantIndex int) string {
	base := in.RegistrationURL

	slug := in.EventSlug
	if slug == "" {
		slug = collapseSpacesToDash(strings.ToLower(in.EventName))
	}

	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}

	utm := map[string]string{
		"utm_source":   "meta",
		"utm_medium":   "paid-social",
		"utm_campaign": campaign,
		"utm_term":     strings.ToLower(collapseSpacesToDash(in.EventName)),
		"utm_content":  fmt.Sprintf("variant-%d", variantIndex+1),
	}

	// Parse the URL so UTM params merge into the existing query and any fragment
	// stays at the very end (a fragment must not be pushed after the query).
	parsed, err := url.Parse(base)
	if err != nil {
		// Fall back to naive concatenation if the URL can't be parsed; this
		// preserves behavior for inputs that already passed validation.
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

	// Normalize a trailing slash on the PATH only. Trimming the raw URL string
	// (the old approach) corrupted URLs whose query or fragment ends in '/'
	// (e.g. "?redirect=/" or "#/"). Trimming the path leaves query/fragment intact.
	if parsed.Path != "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}

	q := parsed.Query()
	for k, v := range utm {
		q.Set(k, v)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

var wsRE = regexp.MustCompile(`\s+`)

// collapseSpacesToDash replaces runs of whitespace with a single dash, matching
// the TS `.replace(/\s+/g, '-')`.
func collapseSpacesToDash(s string) string {
	return wsRE.ReplaceAllString(s, "-")
}

// truncateErr renders an error's message for inclusion in a user-visible step,
// clamping it to a reasonable length without splitting a multi-byte rune.
func truncateErr(err error, max int) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), max)
}

// truncate clamps s to at most max runes, appending an ellipsis when it clips,
// without splitting a multi-byte rune.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func defaultObjective(objective string) string {
	if objective == "" {
		return "traffic"
	}
	return objective
}

// ---------------------------------------------------------------------------
// Public input / result types (mirror MetaCampaignCreateRequest / *Result)
// ---------------------------------------------------------------------------

// AdVariant is a single ad creative variant.
type AdVariant struct {
	PrimaryText string
	Headline    string
	Description string
}

// CampaignInput mirrors MetaCampaignCreateRequest.
type CampaignInput struct {
	EventName       string
	EventSlug       string
	Project         string
	RegistrationURL string
	// Objective is one of awareness|traffic|engagement|leads|conversions.
	// Empty defaults to "traffic".
	Objective      string
	GeoTargets     []string
	BudgetUSD      float64
	LifetimeBudget bool
	StartDate      string // YYYY-MM-DD
	EndDate        string // YYYY-MM-DD
	Placements     Placement
	PixelID        string
	HSToken        string
	Variants       []AdVariant
}

// CampaignResult mirrors MetaCampaignCreateResult.
type CampaignResult struct {
	Platform     string
	CampaignName string
	CampaignID   string
	AdSetName    string
	AdSetID      string
	AdCount      int
	MetaURL      string
	Steps        []string
}

// ---------------------------------------------------------------------------
// CreateCampaign (mirrors executeMetaCampaignCreation)
// ---------------------------------------------------------------------------

// CreateCampaign creates a PAUSED Meta campaign, ad set, and one ad per valid
// variant. It faithfully ports executeMetaCampaignCreation: per-variant ad
// failures are recorded in Steps rather than aborting the whole operation.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	steps := []string{}

	if len(in.Variants) == 0 {
		return nil, fmt.Errorf("at least one ad variant is required for Meta campaign creation")
	}

	validVariants := make([]AdVariant, 0, len(in.Variants))
	for _, v := range in.Variants {
		if strings.TrimSpace(v.PrimaryText) != "" && strings.TrimSpace(v.Headline) != "" {
			validVariants = append(validVariants, v)
		}
	}
	if len(validVariants) == 0 {
		return nil, fmt.Errorf("at least one variant must have non-empty primary text and headline")
	}

	if err := validateRegistrationURL(in.RegistrationURL); err != nil {
		return nil, err
	}

	if math.IsNaN(in.BudgetUSD) || math.IsInf(in.BudgetUSD, 0) || in.BudgetUSD <= 0 {
		return nil, fmt.Errorf("invalid budget: must be a positive number")
	}
	// Reject sub-cent budgets that round to zero cents before any API call: a
	// zero/invalid budget would otherwise be sent to Meta and create a bad ad set.
	budgetCents := int64(math.Round(in.BudgetUSD * 100))
	if budgetCents < 1 {
		return nil, fmt.Errorf("budget too small: must be at least 0.01")
	}

	if !dateRE.MatchString(in.StartDate) {
		return nil, fmt.Errorf("invalid start date format: %s — expected YYYY-MM-DD", in.StartDate)
	}
	if !dateRE.MatchString(in.EndDate) {
		return nil, fmt.Errorf("invalid end date format: %s — expected YYYY-MM-DD", in.EndDate)
	}
	// Reject impossible calendar dates (e.g. 2026-13-40) that pass the shape check.
	if _, err := time.Parse("2006-01-02", in.StartDate); err != nil {
		return nil, fmt.Errorf("invalid start date format: %s — expected YYYY-MM-DD", in.StartDate)
	}
	if _, err := time.Parse("2006-01-02", in.EndDate); err != nil {
		return nil, fmt.Errorf("invalid end date format: %s — expected YYYY-MM-DD", in.EndDate)
	}
	if in.EndDate <= in.StartDate {
		return nil, fmt.Errorf("end date %s must be after start date %s", in.EndDate, in.StartDate)
	}

	// AccountID is required to build every Graph endpoint (/{accountID}/campaigns
	// etc.). An empty AccountID would produce malformed "//campaigns" requests, so
	// fail fast before any mutating call rather than issuing a bad request.
	if strings.TrimSpace(c.account.AccountID) == "" {
		return nil, fmt.Errorf("AccountID is required to create a Meta campaign; configure an ad account for this client")
	}

	// PageID is required for the creative flow (object_story_spec.page_id) and,
	// for some objectives, the promoted_object. Fail fast before any mutating
	// call so a missing PageID doesn't create a paid campaign that can't get ads.
	if strings.TrimSpace(c.account.PageID) == "" {
		return nil, fmt.Errorf("PageID is required to create Meta creatives; configure a Facebook Page for this account")
	}

	// Resolve the objective and validate deterministic inputs (placements and the
	// promoted object) BEFORE the first mutating call, so an input error never
	// creates a paid campaign.
	objective := defaultObjective(in.Objective)
	objParams, ok := objectiveParams[objective]
	if !ok {
		return nil, fmt.Errorf("unknown Meta objective: '%s'. Valid objectives: %s", objective, strings.Join(objectiveKeys(), ", "))
	}
	placementTargeting, err := buildPlacementTargeting(in.Placements)
	if err != nil {
		return nil, err
	}
	promotedObject, err := buildPromotedObject(objective, c.account.PageID, in.PixelID)
	if err != nil {
		return nil, err
	}

	accountID := c.account.AccountID
	label := c.account.Label
	if label == "" {
		label = accountID
	}

	// Step 1: Verify account access (non-fatal, mirrors TS try/catch).
	if err := c.doRequest(ctx, http.MethodGet, "/"+accountID+"?fields=name,account_status", nil, &map[string]any{}); err != nil {
		steps = append(steps, fmt.Sprintf("Account verification warning: %s", truncateErr(err, 300)))
	} else {
		steps = append(steps, fmt.Sprintf("Account verified: %s (%s)", label, accountID))
	}

	// Step 2: geo filtering + campaign creation.
	allGeo := validateGeoTargets(in.GeoTargets)
	geoCountries := make([]string, 0, len(allGeo))
	skippedGeos := make([]string, 0)
	for _, g := range allGeo {
		if regulatedCountries[g] {
			skippedGeos = append(skippedGeos, g)
		} else {
			geoCountries = append(geoCountries, g)
		}
	}
	if len(geoCountries) == 0 {
		return nil, fmt.Errorf("meta campaign skipped: selected geo targets (%s) require manual compliance declaration in Meta Ads Manager. Add at least one non-regulated country or complete the declaration first", strings.Join(skippedGeos, ", "))
	}
	if len(skippedGeos) > 0 {
		steps = append(steps, fmt.Sprintf("Geo targets skipped (require regional compliance declaration in Meta Ads Manager): %s", strings.Join(skippedGeos, ", ")))
	}

	campaignName := buildCampaignName(in, geoCountries)

	var campaignResp createResponse
	err = c.doRequest(ctx, http.MethodPost, "/"+accountID+"/campaigns", map[string]any{
		"name":                            campaignName,
		"objective":                       objParams.CampaignObjective,
		"status":                          "PAUSED",
		"special_ad_categories":           []string{},
		"is_adset_budget_sharing_enabled": false,
	}, &campaignResp)
	if err != nil {
		return nil, err
	}
	campaignID := campaignResp.ID
	if campaignID == "" {
		return nil, fmt.Errorf("meta campaign creation succeeded but returned no campaign ID")
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (%s, PAUSED)", campaignID, objectiveLabel(objective)))

	// Step 3: Ad set (budget, placements, and promoted object were validated up
	// front, before the campaign was created).
	adSetName := fmt.Sprintf("%s - %s", in.EventName, objectiveLabel(objective))

	targeting := map[string]any{"geo_locations": map[string]any{"countries": geoCountries}}
	for k, v := range placementTargeting {
		targeting[k] = v
	}

	adSetBody := map[string]any{
		"name":              adSetName,
		"campaign_id":       campaignID,
		"status":            "PAUSED",
		"billing_event":     "IMPRESSIONS",
		"optimization_goal": objParams.OptimizationGoal,
		"bid_strategy":      "LOWEST_COST_WITHOUT_CAP",
		"targeting":         targeting,
		"start_time":        in.StartDate + "T00:00:00+0000",
		"end_time":          in.EndDate + "T23:59:59+0000",
	}

	if promotedObject != nil {
		adSetBody["promoted_object"] = promotedObject
	}

	if in.LifetimeBudget {
		adSetBody["lifetime_budget"] = budgetCents
	} else {
		adSetBody["daily_budget"] = budgetCents
	}

	var adSetResp createResponse
	if err := c.doRequest(ctx, http.MethodPost, "/"+accountID+"/adsets", adSetBody, &adSetResp); err != nil {
		return nil, err
	}
	adSetID := adSetResp.ID
	if adSetID == "" {
		return nil, fmt.Errorf("meta ad set creation succeeded but returned no ad set ID")
	}
	budgetLabel := "daily"
	if in.LifetimeBudget {
		budgetLabel = "lifetime"
	}
	steps = append(steps, fmt.Sprintf("Ad set created: %s ($%.2f %s, geo: %s)", adSetID, in.BudgetUSD, budgetLabel, strings.Join(geoCountries, ", ")))

	// Step 4: creative + ad per variant (per-variant failures are non-fatal).
	adCount := 0
	for i, variant := range validVariants {
		utmURL := buildUTMURL(in, i)

		adID, creativeID, verr := c.createVariantAd(ctx, in, variant, adSetID, utmURL, i)
		if verr != nil {
			steps = append(steps, fmt.Sprintf("Ad %d failed: %s", i+1, truncateErr(verr, 300)))
			continue
		}
		adCount++
		steps = append(steps, fmt.Sprintf("Ad %d created: %s (creative: %s) → %s", i+1, adID, creativeID, utmURL))
	}

	if adCount == 0 && len(in.Variants) > 0 {
		steps = append(steps, "No ads could be created — create them manually in Meta Ads Manager")
	}

	return &CampaignResult{
		Platform:     "meta-ads",
		CampaignName: campaignName,
		CampaignID:   campaignID,
		AdSetName:    adSetName,
		AdSetID:      adSetID,
		AdCount:      adCount,
		MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.Replace(accountID, "act_", "", 1)),
		Steps:        steps,
	}, nil
}

// createVariantAd creates the adcreative and ad for one variant, returning the
// ad id and creative id.
func (c *Client) createVariantAd(ctx context.Context, in CampaignInput, variant AdVariant, adSetID, utmURL string, i int) (adID, creativeID string, err error) {
	linkData := map[string]any{
		"link":    utmURL,
		"message": variant.PrimaryText,
		"name":    variant.Headline,
		"call_to_action": map[string]any{
			"type":  "LEARN_MORE",
			"value": map[string]any{"link": utmURL},
		},
	}
	if variant.Description != "" {
		linkData["description"] = variant.Description
	}

	var creativeResp createResponse
	if err = c.doRequest(ctx, http.MethodPost, "/"+c.account.AccountID+"/adcreatives", map[string]any{
		"name": fmt.Sprintf("%s - Variant %d", in.EventName, i+1),
		"object_story_spec": map[string]any{
			"page_id":   c.account.PageID,
			"link_data": linkData,
		},
	}, &creativeResp); err != nil {
		return "", "", err
	}
	if creativeResp.ID == "" {
		return "", "", fmt.Errorf("creative creation returned no ID")
	}

	var adResp createResponse
	if err = c.doRequest(ctx, http.MethodPost, "/"+c.account.AccountID+"/ads", map[string]any{
		"name":     fmt.Sprintf("%s - Ad %d", in.EventName, i+1),
		"adset_id": adSetID,
		"creative": map[string]any{"creative_id": creativeResp.ID},
		"status":   "PAUSED",
	}, &adResp); err != nil {
		return "", "", err
	}
	if adResp.ID == "" {
		return "", "", fmt.Errorf("ad creation returned no ID")
	}
	return adResp.ID, creativeResp.ID, nil
}

func objectiveKeys() []string {
	// Stable order matching the TS objective set.
	return []string{"awareness", "traffic", "engagement", "leads", "conversions"}
}
