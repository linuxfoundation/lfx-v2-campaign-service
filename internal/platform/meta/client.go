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
	"unicode/utf8"
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
	// maxResponseBody bounds how much of any response body is read into memory,
	// far above any legitimate Graph API response, to prevent memory exhaustion
	// while not truncating a normal success or error body.
	maxResponseBody = 10 << 20 // 10 MiB
	// adSetStartBuffer is added to "now" when a campaign starts today, so the ad
	// set start_time isn't already in the past by the time Meta receives it.
	adSetStartBuffer = 5 * time.Minute
	// Per-variant copy limits (in runes), mirroring the repo contract in
	// docs/api-catalog.md. Over-limit copy is rejected up front so it fails before
	// any paid campaign/ad-set resource is created rather than at creative
	// creation (which is non-fatal and would leave an orphaned paid campaign).
	maxPrimaryTextChars = 125
	maxHeadlineChars    = 40
	maxDescriptionChars = 30
	// maxCreativeNameChars is Meta's cap on an ad-creative name. The creative name
	// is composed ("<EventName> - Variant N"), so the COMPOSED value is validated
	// up front against this before any mutating call.
	maxCreativeNameChars = 255
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
// Mirrors META_OBJECTIVE_PARAMS from @lfx-one/shared/constants, WITH ONE
// INTENTIONAL EXCEPTION: "leads" maps to OUTCOME_TRAFFIC/LINK_CLICKS/none here
// rather than the shared LEAD_GENERATION/page_id, because this client builds only
// a website-click creative and never constructs an on-Facebook instant lead form
// (see the "leads" entry's comment and LFXV2-2665).
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
	// "leads" INTENTIONALLY DIVERGES from the @lfx-one/shared TS contract
	// (campaign.constants.ts META_OBJECTIVE_PARAMS), which maps leads ->
	// LEAD_GENERATION with a page_id promoted object. This is a deliberate,
	// documented divergence — NOT an oversight or a bug. That shared mapping assumes
	// an on-Facebook instant lead form: LEAD_GENERATION requires the ad's creative
	// to reference a lead_gen_form_id (an instant form). This Go client only builds
	// a website-click creative (object_story_spec.link_data pointing at the
	// registration URL — see createVariantAd); it never constructs an instant lead
	// form, so LEAD_GENERATION would fail at ad-set/ad creation.
	//
	// The interim mapping runs a WEBSITE-TRAFFIC campaign: OUTCOME_TRAFFIC
	// optimizing for LINK_CLICKS to the registration (lead-capture) URL, with no
	// promoted object. OUTCOME_TRAFFIC is the objective that cleanly supports
	// LINK_CLICKS optimization with NO pixel/promoted-object requirement, so the
	// ad-set POST always succeeds (a consistent, spendable configuration
	// end-to-end). OUTCOME_LEADS + LINK_CLICKS is avoided precisely because Meta
	// requires a pixel_id + custom_event_type for that pairing, which this interim
	// flow does not supply — it would create the campaign then fail at the ad set,
	// orphaning a paid resource.
	//
	// Full LEAD_GENERATION / instant-form (or OUTCOME_LEADS + pixel) parity with the
	// shared TS contract is INTENTIONALLY OUT OF SCOPE for this PR and tracked as a
	// follow-up (LFXV2-2665).
	"leads": {
		CampaignObjective:  "OUTCOME_TRAFFIC",
		OptimizationGoal:   "LINK_CLICKS",
		PromotedObjectType: PromotedObjectNone,
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
	// CurrencyOffset is an OPTIONAL override of the ad account's minor-unit
	// offset: the factor that converts a whole-currency-unit budget into
	// the minor units Meta expects. Meta budgets are ALWAYS expressed in minor
	// units scaled by the ACCOUNT's currency, which is NOT universally 100 —
	// zero-decimal currencies such as JPY, KRW, and CLP use an offset of 1 (no
	// minor unit), while most (USD, EUR, GBP) use 100.
	//
	// When left unset (zero), CreateCampaign fetches the account's ISO 4217 currency
	// CODE from Meta during the account preflight (GET on the ad-account object with
	// fields=name,account_status,currency) BEFORE any mutating call and DERIVES the
	// offset from it via a reference table (100 for two-decimal currencies, 1 for
	// zero-decimal ones like JPY/KRW/CLP). The AdAccount node does NOT expose a
	// currency_offset field — only the ISO code — so the scale is derived, not
	// fetched. If the currency is unknown or absent, CreateCampaign fails BEFORE
	// mutation rather than guessing 100 — a silent default would encode a
	// zero-decimal-currency (JPY/KRW/CLP) budget 100× too high, and a warning after
	// resource creation cannot prevent that budget from being activated.
	//
	// A caller MAY set this field to a positive value as a FALLBACK for when the
	// account preflight can't identify the currency. The account currency is
	// authoritative: if the preflight returns a RECOGNIZED currency whose true
	// offset DIFFERS from this explicit value, CreateCampaign REJECTS the request
	// (a stale override would mis-scale the budget, e.g. 100 on a JPY account). The
	// explicit value is only used when the preflight fails or its currency is not
	// in the supported-currency map. The preflight GET always runs (it also
	// verifies account access). A negative value is rejected as malformed.
	CurrencyOffset int64
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
	return func(c *Client) {
		// Ignore a nil client so the safe default installed by NewClient isn't
		// replaced with nil (which would panic on the next request).
		if h != nil {
			c.httpClient = h
		}
	}
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
	// Trim credential/account fields once at construction so validation (which
	// uses TrimSpace) and request building (which used the raw values in URLs like
	// "/"+accountID) can't disagree — surrounding whitespace would otherwise pass
	// validation but produce malformed requests.
	creds.AccessToken = strings.TrimSpace(creds.AccessToken)
	account.AccountID = strings.TrimSpace(account.AccountID)
	account.PageID = strings.TrimSpace(account.PageID)
	// NOTE: CurrencyOffset is NOT coerced here. It is not defaulted in NewClient so
	// the zero value remains distinguishable as "unset": when unset, CreateCampaign
	// derives the offset from the account's ISO currency code fetched during the
	// account preflight (see AccountConfig.CurrencyOffset). A negative offset is
	// rejected as malformed at budget-conversion time.
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

// accountPreflight models the fields read from the ad-account object during the
// account preflight (GET /act_<id>?fields=name,account_status,currency). The
// AdAccount node exposes the ISO 4217 currency CODE only — it does NOT expose a
// currency_offset field (only the separate Currency node does). The minor-unit
// multiplier used to encode the budget is derived from this code via
// currencyMinorUnitOffset before any mutating call.
type accountPreflight struct {
	Name          string `json:"name"`
	AccountStatus int    `json:"account_status"`
	Currency      string `json:"currency"`
}

// metaAccountStatusActive is Meta's account_status value for an ACTIVE ad account.
const metaAccountStatusActive = 1

// inactiveAccountStatusLabels maps the well-known non-active Meta account_status
// values to a human-readable reason. A campaign created against an account in one
// of these states would only fail at a later mutating call, so CreateCampaign
// refuses BEFORE any paid resource is created when the preflight reports one of
// these. account_status 0 (absent/unreported) and any value not listed here are
// treated as "not known-bad" and allowed through — this is a conservative block
// on definitively-disabled accounts, not a positive allowlist.
var inactiveAccountStatusLabels = map[int]string{
	2:   "disabled",
	3:   "unsettled",
	7:   "pending risk review",
	8:   "pending settlement",
	9:   "in grace period",
	100: "pending closure",
	101: "closed",
	// NOTE: 201 (ANY_ACTIVE) and 202 (ANY_CLOSED) are Meta AGGREGATE/filter values,
	// not per-account statuses — a real ad-account's account_status is never 201/202.
	// 201 in particular denotes an ACTIVE aggregate, so listing it here would reject
	// an active account. They are intentionally omitted from this known-bad map.
}

// currencyMinorUnitOffset is the AUTHORITATIVE map of the Meta ad-account
// currencies this client supports, each mapped to the factor that converts a
// whole-currency-unit budget into the minor units Meta expects. This map — NOT a
// default — is the single source of truth: a code that is not present is treated
// as UNSUPPORTED and fails before any mutating call (see currencyOffsetFor).
//
// The AdAccount node exposes only the ISO 4217 currency CODE (not a
// currency_offset field), so the offset is derived from this map rather than
// fetched. Two groups of entries:
//
//   - offset 1: the zero-decimal (no minor unit) currencies. Meta bills these in
//     whole units, so a budget must NOT be multiplied by 100 for them (the
//     JPY/KRW 100× over-spend bug).
//   - offset 100: the common two-decimal currencies Meta supports.
//
// A blank/absent code, or a well-formed-but-unrecognized one (e.g. a new or
// malformed code like "ZZZ"), returns ok=false from currencyOffsetFor so the
// caller fails BEFORE mutation instead of guessing 100 — which could silently
// encode a zero-decimal budget 100× too high. When a genuinely-supported currency
// is missing here, add it to this map (with the correct factor) rather than
// relying on a fall-through default.
//
// Three-decimal currencies are intentionally NOT special-cased: Meta bills ads in
// whole minor units, so two-decimal vs zero-decimal is the distinction that
// matters for budget encoding here — a three-decimal code is simply absent (and
// therefore rejected) until it is added deliberately with a verified factor.
var currencyMinorUnitOffset = map[string]int64{
	// Zero-decimal currencies (offset 1): no minor unit, billed in whole units.
	"BIF": 1, // Burundian Franc
	"CLP": 1, // Chilean Peso
	"DJF": 1, // Djiboutian Franc
	"GNF": 1, // Guinean Franc
	"ISK": 1, // Icelandic Krona
	"JPY": 1, // Japanese Yen
	"KMF": 1, // Comorian Franc
	"KRW": 1, // South Korean Won
	"MGA": 1, // Malagasy Ariary (5-subunit, but Meta treats as integer minor)
	"PYG": 1, // Paraguayan Guarani
	"RWF": 1, // Rwandan Franc
	"UGX": 1, // Ugandan Shilling
	"VND": 1, // Vietnamese Dong
	"VUV": 1, // Vanuatu Vatu
	"XAF": 1, // Central African CFA Franc
	"XOF": 1, // West African CFA Franc
	"XPF": 1, // CFP Franc
	// These are ALSO offset-1 for the Meta Marketing API despite having minor
	// units in general ISO usage — Meta bills ad amounts in whole units for them.
	// Verified against developers.facebook.com/docs/marketing-api/currencies.
	"IDR": 1, // Indonesian Rupiah
	"HUF": 1, // Hungarian Forint
	"COP": 1, // Colombian Peso
	"CRC": 1, // Costa Rican Colon
	"TWD": 1, // New Taiwan Dollar

	// Two-decimal currencies (offset 100): the common ISO 4217 codes Meta
	// supports as ad-account currencies. A code outside this set is rejected, not
	// assumed to be two-decimal.
	"USD": 100, // US Dollar
	"EUR": 100, // Euro
	"GBP": 100, // Pound Sterling
	"AUD": 100, // Australian Dollar
	"CAD": 100, // Canadian Dollar
	"CHF": 100, // Swiss Franc
	"CNY": 100, // Chinese Yuan
	"DKK": 100, // Danish Krone
	"HKD": 100, // Hong Kong Dollar
	"INR": 100, // Indian Rupee
	"MXN": 100, // Mexican Peso
	"NOK": 100, // Norwegian Krone
	"NZD": 100, // New Zealand Dollar
	"PLN": 100, // Polish Zloty
	"SEK": 100, // Swedish Krona
	"SGD": 100, // Singapore Dollar
	"THB": 100, // Thai Baht
	"TRY": 100, // Turkish Lira
	"ZAR": 100, // South African Rand
	"BRL": 100, // Brazilian Real
	"ILS": 100, // Israeli New Shekel
	"PHP": 100, // Philippine Peso
	"MYR": 100, // Malaysian Ringgit
	"AED": 100, // UAE Dirham
	"SAR": 100, // Saudi Riyal
	"CZK": 100, // Czech Koruna
	"RON": 100, // Romanian Leu
	"ARS": 100, // Argentine Peso
	"BDT": 100, // Bangladeshi Taka
	"BOB": 100, // Bolivian Boliviano
	"DZD": 100, // Algerian Dinar
	"EGP": 100, // Egyptian Pound
	"GTQ": 100, // Guatemalan Quetzal
	"HNL": 100, // Honduran Lempira
	"KES": 100, // Kenyan Shilling
	"MOP": 100, // Macanese Pataca
	"NGN": 100, // Nigerian Naira
	"NIO": 100, // Nicaraguan Cordoba
	"PEN": 100, // Peruvian Sol
	"PKR": 100, // Pakistani Rupee
	"QAR": 100, // Qatari Riyal
	"UYU": 100, // Uruguayan Peso
}

// currencyOffsetFor derives the minor-unit multiplier for an ISO 4217 currency
// code returned by the account preflight, using currencyMinorUnitOffset as the
// authoritative supported-currency set. It returns (offset, true) only for a code
// present in that map, and (0, false) for a blank/absent code OR a well-formed
// code that is not in the map (an unknown/malformed currency such as "ZZZ"). The
// caller must fail before mutation on a false result rather than guessing 100 —
// which for a zero-decimal currency would over-encode the budget 100×.
func currencyOffsetFor(currency string) (int64, bool) {
	code := strings.ToUpper(strings.TrimSpace(currency))
	if code == "" {
		return 0, false
	}
	off, ok := currencyMinorUnitOffset[code]
	return off, ok
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

// graphRateLimitCodes are Graph/Marketing API error codes that indicate
// throttling, which Meta commonly returns as an HTTP 400 (not a 429): 4 =
// application request-limit reached, 17 = user request-limit reached, 32 =
// page-level throttling, 341 = temporary app-level limit, 613 = ad-account
// rate limit, 80004 = ad-account/business-use-case throttling (Marketing API).
// These are retried with the same backoff as a 429.
var graphRateLimitCodes = map[int]bool{4: true, 17: true, 32: true, 341: true, 613: true, 80004: true}

// APIError is returned when the Meta API responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	// Message is the Graph API error message when present, else the raw body.
	Message string
	// Type, Code, and FBTraceID carry the Graph error envelope's diagnostic
	// fields. They let callers distinguish invalid-params from auth failures
	// (which often share HTTP 400/400) and quote Meta's trace id in support
	// tickets. They are zero-valued when the body isn't a Graph error envelope.
	Type      string
	Code      int
	FBTraceID string
}

func (e *APIError) Error() string {
	// Mirror the TS behavior of not leaking full bodies to callers while still
	// surfacing status; include the parsed message when available, plus the Graph
	// diagnostic fields (type/code/fbtrace_id) when present — fbtrace_id in
	// particular is essential when opening a Meta support ticket.
	var b strings.Builder
	if e.Message != "" {
		fmt.Fprintf(&b, "meta API request failed (%d): %s", e.StatusCode, e.Message)
	} else {
		fmt.Fprintf(&b, "meta API request failed (%d) with no error details in the response body", e.StatusCode)
	}
	if e.Type != "" {
		fmt.Fprintf(&b, " (type: %s", e.Type)
		if e.Code != 0 {
			fmt.Fprintf(&b, ", code: %d", e.Code)
		}
		b.WriteString(")")
	} else if e.Code != 0 {
		fmt.Fprintf(&b, " (code: %d)", e.Code)
	}
	if e.FBTraceID != "" {
		fmt.Fprintf(&b, " [fbtrace_id: %s]", e.FBTraceID)
	}
	return b.String()
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

		// Read one byte past the cap so a truncation is detectable: io.LimitReader
		// returns EOF (not an error) at the limit, so an oversized body would
		// otherwise be silently truncated and mis-parsed as a valid short response.
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
		if readErr == nil && int64(len(raw)) > maxResponseBody {
			_ = resp.Body.Close()
			return fmt.Errorf("meta API %s %s: response exceeds %d bytes", method, path, maxResponseBody)
		}
		retryAfter := c.parseRetryAfter(resp)
		status := resp.StatusCode
		_ = resp.Body.Close()

		// Meta reports throttling either as HTTP 429 or, commonly, as HTTP 400 with
		// a Graph error envelope whose code is a known rate-limit code. Treat both
		// as retryable with the same bounded backoff.
		var env graphErrorEnvelope
		_ = json.Unmarshal(raw, &env)
		throttled := status == http.StatusTooManyRequests ||
			(status < 200 || status >= 300) && env.Error != nil && graphRateLimitCodes[env.Error.Code]

		// A read error (e.g. connection closed early on a mismatched Content-Length)
		// must not be treated as a complete response: even if the partial body
		// happens to parse, propagate the error rather than reporting a false
		// success. But do NOT short-circuit a throttled response we're about to
		// retry (its body is discarded anyway) — only fail when we would otherwise
		// consume this response as final.
		if readErr != nil && (!throttled || attempt >= retryMax) {
			return fmt.Errorf("meta API %s %s: read response body: %w", method, path, readErr)
		}

		if throttled && attempt < retryMax {
			if retryAfter > 0 {
				// The server DECLARED when the limit clears. If that exceeds our cap,
				// sleeping only maxRetryWait would retry while Meta is still throttling
				// — burning attempts and stalling this synchronous flow — so ABORT with
				// the rate-limit error instead of clamping (mirrors the twitter/reddit
				// clients). Only when the server gives no usable reset do we fall back to
				// a capped exponential backoff.
				if retryAfter > maxRetryWait {
					// Preserve the Graph envelope's diagnostics (Type/Code/FBTraceID and
					// original message) on the abort — support may need them exactly when a
					// rate limit is hit — rather than discarding them for a bare message.
					abortErr := &APIError{
						StatusCode: status, Method: method, Path: path,
						Message: fmt.Sprintf("rate-limit reset (Retry-After: %s) exceeds max wait %s; aborting", retryAfter, maxRetryWait),
					}
					if env.Error != nil {
						abortErr.Type = env.Error.Type
						abortErr.Code = env.Error.Code
						abortErr.FBTraceID = env.Error.FBTraceID
						if env.Error.Message != "" {
							abortErr.Message = fmt.Sprintf("%s (Graph: %s)", abortErr.Message, env.Error.Message)
						}
					}
					return abortErr
				}
				if err := sleepCtx(ctx, retryAfter); err != nil {
					return err
				}
				continue
			}
			// No server-declared reset: capped exponential backoff.
			wait := c.retryBaseDelay * time.Duration(1<<uint(attempt))
			if wait > maxRetryWait {
				wait = maxRetryWait
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		}

		if status < 200 || status >= 300 {
			apiErr := &APIError{StatusCode: status, Method: method, Path: path}
			if env.Error != nil {
				// Preserve the Graph envelope's diagnostic fields so callers can
				// distinguish invalid-params vs auth failures and quote the trace id.
				apiErr.Type = env.Error.Type
				apiErr.Code = env.Error.Code
				apiErr.FBTraceID = env.Error.FBTraceID
			}
			if env.Error != nil && env.Error.Message != "" {
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
		Message: fmt.Sprintf("exhausted %d retries after rate limiting", retryMax)}
}

// parseRetryAfter returns how long to wait before retrying a 429, or 0 if no
// usable header is present. Meta returns Retry-After either as a delay in seconds
// or as an HTTP-date; both forms are honored. Never returns a negative duration.
func (c *Client) parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	// Delay-seconds form. ParseInt into an int64 (not Atoi, whose platform int can
	// overflow on 32-bit and silently drop a real, if outsized, value) and CLAMP
	// before multiplying: time.Duration(n)*time.Second wraps NEGATIVE for n beyond
	// ~9.2e9, which would make the caller retry far too early. Any n strictly above
	// the max-wait ceiling (in seconds) already exceeds the cap, so report a
	// duration just over maxRetryWait and let the caller's own cap apply — never
	// perform the wrapping multiply. Mirrors internal/platform/twitter/client.go.
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(maxRetryWait/time.Second) {
			return maxRetryWait + time.Second
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.timeNow()); d > 0 {
			// Clamp an outsized HTTP-date reset the same way, so a far-future date
			// can't wait past the point of usefulness (the caller also caps to
			// maxRetryWait, but keep the two branches consistent).
			if d > maxRetryWait {
				return maxRetryWait + time.Second
			}
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

// accountIDRE matches a Meta ad-account id in its documented "act_<digits>" form.
// AccountID is interpolated into every Graph path ("/"+accountID+"/campaigns"),
// so a non-empty check is not enough: a value carrying '/', '?', '#', '..', or
// whitespace could redirect a request to a different endpoint. Anchored so the
// whole value must match. Mirrors the anchored-regex approach in
// internal/platform/twitter/client.go (accountIDRe).
var accountIDRE = regexp.MustCompile(`^act_[0-9]+$`)

// numericIDRE matches a purely numeric Meta object id (Page id, Pixel id). Meta
// object ids are decimal strings; validating the format up front stops a malformed
// id (e.g. "PIX9") from creating a campaign/ad set that then fails at creative or
// promoted-object time, leaving an orphaned paid resource.
var numericIDRE = regexp.MustCompile(`^[0-9]+$`)

func validateRegistrationURL(raw string) error {
	parsed, err := url.Parse(raw)
	// Require an absolute URL with a real hostname. parsed.Host can be a
	// port-only authority (e.g. "https://:443" parses to Host==":443" with an
	// empty Hostname()), which is not a valid destination — check Hostname().
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return fmt.Errorf("registration URL is not a valid URL")
	}
	// Reject embedded userinfo (user[:password]@host): an ad destination never
	// needs URL credentials, and buildUTMURL would otherwise forward the password
	// to Meta as the creative click URL and echo it in the success step, leaking a
	// basic-auth secret. Mirrors the reddit client's validateRegistrationURL.
	if parsed.User != nil {
		return fmt.Errorf("registration URL must not contain embedded credentials (userinfo)")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("registration URL must use HTTPS")
	}
	// url.Parse does not validate the query. buildUTMURL rebuilds the URL via
	// u.Query() (which SILENTLY drops a pair it can't parse — e.g. one containing an
	// unescaped ';' or bad percent-encoding), so the ad's click URL could differ
	// from what the caller supplied. Reject a query that ParseQuery can't cleanly
	// parse, before any mutating call.
	if _, qerr := url.ParseQuery(parsed.RawQuery); qerr != nil {
		return fmt.Errorf("registration URL has a malformed query string")
	}
	return nil
}

// validateGeoTargets uppercases, trims, and filters to ISO-2 codes; defaults to
// ["US"] when nothing valid remains (mirrors validateGeoTargets).
func validateGeoTargets(geoTargets []string) []string {
	valid := make([]string, 0, len(geoTargets))
	for _, g := range geoTargets {
		up := strings.ToUpper(strings.TrimSpace(g))
		// Check shape and ISO 3166-1 alpha-2 membership (so a well-shaped but bogus
		// code like "XX"/"ZZ" is dropped), and exclude countries Meta does not allow
		// as ad targets (see metaIneligibleCountries) — ISO membership is not the
		// same as Meta targeting eligibility.
		if geoCodeRE.MatchString(up) && iso3166Alpha2[up] && !metaIneligibleCountries[up] {
			valid = append(valid, up)
		}
	}
	if len(valid) == 0 {
		return []string{"US"}
	}
	return valid
}

// metaIneligibleCountries are ISO 3166-1 codes that are NOT valid Meta ad-targeting
// countries; ISO membership alone would otherwise let them through and be rejected
// only after the campaign is created. This is deliberately a curated exclusion list
// rather than a positive allowlist: ISO 3166-1 assigns codes for uninhabited and
// special territories that carry no Meta ad market, and for a handful of countries
// Meta/OFAC exclude on policy grounds. It covers the two known leak classes:
//
//  1. Policy/sanctions exclusions. CU/IR/KP remain under active comprehensive OFAC
//     sanctions programs. RU is excluded because Meta's ads policy bans targeting
//     Russia; SY is kept excluded pending confirmation of Meta's current targeting
//     eligibility (OFAC terminated its comprehensive Syria program effective
//     2025-07-01, so that is no longer the basis).
//  2. Uninhabited / non-targetable territories that are assigned ISO codes but are
//     not Meta ad-geolocation countries (no resident audience to target), so a
//     campaign targeting them would be created and then fail at the ad-set step.
//
// NOTE: this is best-effort, not Meta's authoritative ad-geolocation set. If a
// still-ISO-valid but non-targetable code slips through, Meta rejects the ad-set
// POST (after the PAUSED campaign is created) and the returned error surfaces the
// created campaign id for cleanup. A maintained targetable-country allowlist would
// be stricter; that is intentionally deferred to keep this list auditable.
var metaIneligibleCountries = map[string]bool{
	"CU": true, // Cuba (comprehensively sanctioned)
	"IR": true, // Iran (comprehensively sanctioned)
	"KP": true, // North Korea (comprehensively sanctioned)
	"RU": true, // Russia (Meta ads policy prohibits targeting; not OFAC-comprehensive)
	"SY": true, // Syria (Meta ads-eligibility caution; not OFAC-comprehensive as of 2025-07-01)
	// Uninhabited / non-targetable ISO territories (no Meta ad market).
	"AQ": true, // Antarctica (no resident population)
	"BV": true, // Bouvet Island (uninhabited)
	"HM": true, // Heard Island and McDonald Islands (uninhabited)
	"TF": true, // French Southern Territories (no permanent population)
	"GS": true, // South Georgia and the South Sandwich Islands (no permanent population)
	"UM": true, // United States Minor Outlying Islands (no permanent population)
}

// iso3166Alpha2 is the set of assigned ISO 3166-1 alpha-2 country codes, used to
// reject well-shaped but non-existent codes before they reach Meta.
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
	params, ok := objectiveParams[objective]
	if !ok {
		// Defensive: an unknown objective should never reach here (CreateCampaign
		// validates it up front), but silently treating it as "no promoted object"
		// would be a subtle mis-config if a future caller/refactor bypasses that.
		return nil, fmt.Errorf("unknown objective %q", objective)
	}
	switch params.PromotedObjectType {
	case PromotedObjectPageID:
		return map[string]any{"page_id": pageID}, nil
	case PromotedObjectPixelID:
		trimmed := strings.TrimSpace(pixelID)
		if trimmed == "" {
			return nil, fmt.Errorf("pixelID must be a non-empty string for '%s' objective", objective)
		}
		// An empty-only check lets a malformed pixel id (e.g. "PIX9") through; the
		// campaign would then be created and Meta would reject the promoted object at
		// ad-set creation, leaving an orphan. Meta Pixel ids are numeric, so validate
		// the format here — buildPromotedObject runs before any mutating call.
		if !numericIDRE.MatchString(trimmed) {
			return nil, fmt.Errorf("pixelID %q is malformed for '%s' objective: Meta Pixel IDs are numeric strings", trimmed, objective)
		}
		return map[string]any{"pixel_id": trimmed, "custom_event_type": "PURCHASE"}, nil
	default:
		return nil, nil
	}
}

func buildPlacementTargeting(over Placement) (map[string]any, error) {
	pl := mergePlacements(over)

	var publisherPlatforms, facebookPositions, instagramPositions []string
	// Track membership in a set so addPlatform is O(1) rather than a linear scan
	// of publisherPlatforms on every call (the slice preserves insertion order).
	seenPlatforms := make(map[string]struct{})
	addPlatform := func(p string) {
		if _, ok := seenPlatforms[p]; !ok {
			seenPlatforms[p] = struct{}{}
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
		// Messenger Inbox was removed as a Meta Ads placement in November 2025, so
		// "messenger" / "messenger_home" is not valid on Graph API v25.0: it would
		// pass here and then fail at the ad-set call, after the campaign (a paid
		// resource) already exists. Reject up front instead.
		return nil, fmt.Errorf("messengerInbox placement is no longer supported by Meta Ads (removed November 2025); do not enable it")
	}

	if len(publisherPlatforms) == 0 {
		return nil, fmt.Errorf("at least one placement must be enabled (facebookFeed, instagramFeed, stories, reels, or audienceNetwork)")
	}

	targeting := map[string]any{"publisher_platforms": publisherPlatforms}
	if len(facebookPositions) > 0 {
		targeting["facebook_positions"] = facebookPositions
	}
	if len(instagramPositions) > 0 {
		targeting["instagram_positions"] = instagramPositions
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
// geo-filtered) targets to resolve the region segment. The caller (CreateCampaign)
// validates in.Project is non-empty before this is reached, so there is no
// silent-substitution fallback here: the naming contract's Project segment is the
// caller-supplied canonical LFX slug (docs/api-catalog.md). Substituting a
// placeholder (e.g. "tlf") for an omitted project could mis-attribute a
// non-Linux-Foundation campaign to the wrong project.
func buildCampaignName(in CampaignInput, geoTargets []string) string {
	// Segments are trimmed as well as pipe-stripped: validation TrimSpaces its
	// checks, so " cncf " passes validation — but the attribution pipeline joins
	// the Project segment exactly, and a padded slug would not match.
	event := strings.ReplaceAll(strings.TrimSpace(in.EventName), "|", "-")
	region := resolveRegion(geoTargets)
	objective := objectiveLabel(defaultObjective(in.Objective))
	project := strings.ReplaceAll(strings.TrimSpace(in.Project), "|", "-")
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
// without splitting a multi-byte rune. It walks runes only up to the cutoff
// rather than converting the whole string to []rune, so surfacing a large
// upstream error body (up to maxResponseBody) doesn't allocate/scan all of it.
func truncate(s string, max int) string {
	count := 0
	for i := range s {
		if count == max {
			return s[:i] + "…"
		}
		count++
	}
	// Fewer than (or exactly) max runes: no clipping, return as-is.
	return s
}

// adSetStartTime returns the ad set start_time (RFC3339-ish, Meta format) for a
// start date. When the start date is today, 00:00 UTC is already in the past by
// the time the request reaches Meta (which rejects a past start_time), so use
// now + a small buffer instead; otherwise use start-of-day for the future date.
func adSetStartTime(startDate, now time.Time) string {
	startOfDay := startDate.UTC().Truncate(24 * time.Hour)
	buffered := now.UTC().Add(adSetStartBuffer)
	t := startOfDay
	if buffered.After(startOfDay) {
		t = buffered
	}
	return t.Format("2006-01-02T15:04:05-0700")
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
	// Objective is one of awareness|traffic|engagement|leads|conversions. Empty
	// defaults to "traffic". "leads" runs an interim website-traffic campaign
	// (OUTCOME_TRAFFIC optimizing for LINK_CLICKS to the registration URL); it does
	// not build an on-Facebook instant lead form. Full LEAD_GENERATION / instant-
	// form parity (and status-toggling + analytics) are deferred to LFXV2-2665.
	Objective  string
	GeoTargets []string
	// Budget is the budget amount in whole units of the ad ACCOUNT's currency.
	// IMPORTANT: this is NOT a USD amount and the client performs NO foreign-
	// exchange conversion. Meta bills the ad set in the account's own currency, so
	// the caller must supply an amount already denominated in that currency. The
	// value is converted to minor units by multiplying by the account's minor-unit
	// offset (resolved from AccountConfig.CurrencyOffset when set, otherwise derived
	// from the ISO currency code fetched during the account preflight; 100 for most
	// currencies, 1 for zero-decimal currencies like JPY) and sent as-is.
	// (Renamed from BudgetUSD:
	// the field never carried FX-converted USD — the old name implied a conversion
	// this client does not do.)
	Budget         float64
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

	// Enforce Meta's per-field copy limits (by rune count) up front, before any
	// mutating call. Over-limit copy passes the blank checks above but would be
	// rejected at (non-fatal) creative creation — after the paid campaign/ad-set
	// already exist — leaving an orphaned campaign with no ads. Fail fast instead.
	for i, v := range validVariants {
		if n := utf8.RuneCountInString(v.PrimaryText); n > maxPrimaryTextChars {
			return nil, fmt.Errorf("variant %d primary text is %d characters; Meta allows at most %d", i+1, n, maxPrimaryTextChars)
		}
		if n := utf8.RuneCountInString(v.Headline); n > maxHeadlineChars {
			return nil, fmt.Errorf("variant %d headline is %d characters; Meta allows at most %d", i+1, n, maxHeadlineChars)
		}
		if n := utf8.RuneCountInString(v.Description); n > maxDescriptionChars {
			return nil, fmt.Errorf("variant %d description is %d characters; Meta allows at most %d", i+1, n, maxDescriptionChars)
		}
		// The ad-creative NAME is composed as "<EventName> - Variant N" and Meta caps
		// ad-creative names at maxCreativeNameChars. Validate the COMPOSED name up
		// front too — a long EventName would otherwise pass the copy checks, create
		// the campaign + ad set, then fail at every creative (orphaning both).
		creativeName := fmt.Sprintf("%s - Variant %d", in.EventName, i+1)
		if n := utf8.RuneCountInString(creativeName); n > maxCreativeNameChars {
			return nil, fmt.Errorf("variant %d ad-creative name is %d characters; Meta allows at most %d (shorten the event name)", i+1, n, maxCreativeNameChars)
		}
	}

	if err := validateRegistrationURL(in.RegistrationURL); err != nil {
		return nil, err
	}

	if math.IsNaN(in.Budget) || math.IsInf(in.Budget, 0) || in.Budget <= 0 {
		return nil, fmt.Errorf("invalid budget: must be a positive number")
	}
	// NOTE: no fixed major-unit budget cap is applied here. A hardcoded ceiling (in
	// whole currency units) wrongly rejected realistic budgets in low-value
	// currencies — e.g. a few-thousand-USD-equivalent budget in VND (offset 1)
	// exceeds a 100M major-unit cap while being a perfectly ordinary spend. The
	// offset-aware overflow guard below (after the account currency offset is
	// resolved) is the authoritative overflow check: it rejects only budgets whose
	// SCALED minor-unit value would exceed int64, which is the value actually sent.
	// A negative explicit offset is malformed and can be rejected here, before any
	// network call. The unset (zero) case is resolved from the account preflight
	// below (Step 1); the minor-unit conversion happens there, once the offset is
	// known but still BEFORE any mutating call.
	if c.account.CurrencyOffset < 0 {
		return nil, fmt.Errorf("meta: AccountConfig.CurrencyOffset must not be negative (100 for most currencies, 1 for zero-decimal like JPY)")
	}

	if !dateRE.MatchString(in.StartDate) {
		return nil, fmt.Errorf("invalid start date format: %s — expected YYYY-MM-DD", in.StartDate)
	}
	if !dateRE.MatchString(in.EndDate) {
		return nil, fmt.Errorf("invalid end date format: %s — expected YYYY-MM-DD", in.EndDate)
	}
	// Reject impossible calendar dates (e.g. 2026-13-40) that pass the shape check.
	startDate, err := time.Parse("2006-01-02", in.StartDate)
	if err != nil {
		return nil, fmt.Errorf("invalid start date format: %s — expected YYYY-MM-DD", in.StartDate)
	}
	if _, err := time.Parse("2006-01-02", in.EndDate); err != nil {
		return nil, fmt.Errorf("invalid end date format: %s — expected YYYY-MM-DD", in.EndDate)
	}
	if in.EndDate <= in.StartDate {
		return nil, fmt.Errorf("end date %s must be after start date %s", in.EndDate, in.StartDate)
	}
	// Reject a start date already in the past (compared by calendar day in UTC):
	// Meta rejects a past schedule, but only after the campaign is created, so
	// fail fast here before any mutating call.
	today := c.timeNow().UTC().Truncate(24 * time.Hour)
	if startDate.Before(today) {
		return nil, fmt.Errorf("start date %s is in the past", in.StartDate)
	}

	// AccountID is required to build every Graph endpoint (/{accountID}/campaigns
	// etc.). An empty AccountID would produce malformed "//campaigns" requests, so
	// fail fast before any mutating call rather than issuing a bad request.
	if strings.TrimSpace(c.account.AccountID) == "" {
		return nil, fmt.Errorf("AccountID is required to create a Meta campaign; configure an ad account for this client")
	}
	// A non-empty check is not enough: AccountID is interpolated into every Graph
	// path, so a value with delimiters ('/', '?', '#'), '..', or control chars
	// could redirect a request to a different endpoint. Validate the documented
	// act_<digits> format before any mutating call. Mirrors twitter/client.go's
	// anchored accountIDRe check.
	if !accountIDRE.MatchString(c.account.AccountID) {
		return nil, fmt.Errorf("AccountID %q is malformed: expected the format act_<digits> (e.g. act_193556282970417)", c.account.AccountID)
	}

	// PageID is required for the creative flow (object_story_spec.page_id) and,
	// for some objectives, the promoted_object. Fail fast before any mutating
	// call so a missing PageID doesn't create a paid campaign that can't get ads.
	if strings.TrimSpace(c.account.PageID) == "" {
		return nil, fmt.Errorf("PageID is required to create Meta creatives; configure a Facebook Page for this account")
	}
	// A non-empty check is not enough: a malformed Page id would pass, then the
	// campaign and ad set get created before the creative fails (non-fatally),
	// leaving orphaned paid resources. Meta Page ids are numeric strings, so
	// validate the format before the first POST.
	if !numericIDRE.MatchString(c.account.PageID) {
		return nil, fmt.Errorf("PageID %q is malformed: Meta Page IDs are numeric strings", c.account.PageID)
	}

	// Project is required: the campaign name's Project segment must be the caller-
	// supplied canonical LFX project slug (docs/api-catalog.md). Reject an empty or
	// whitespace-only Project before any mutating call rather than silently
	// substituting a placeholder (e.g. "tlf"), which could mis-attribute a
	// non-Linux-Foundation campaign to the wrong project.
	if strings.TrimSpace(in.Project) == "" {
		return nil, fmt.Errorf("project is required: supply the canonical LFX project slug for the campaign name's Project segment")
	}

	// EventName is required: it is the base-name segment of every generated name
	// (campaign, ad set, creative, ad) and feeds downstream UTM/attribution. Reject
	// an empty or whitespace-only EventName before any mutating call rather than
	// creating paid resources with an empty base-name segment (e.g. " - Traffic"),
	// which would also break attribution.
	if strings.TrimSpace(in.EventName) == "" {
		return nil, fmt.Errorf("event name is required: supply a non-empty base name for the campaign name and attribution segments")
	}
	// Normalize EventName to its trimmed form for the rest of the flow. Only
	// buildCampaignName trims internally; the ad-set/creative/ad names and the UTM
	// builder (utm_term) consume in.EventName raw, so a padded value like
	// " KubeCon EU " would otherwise yield inconsistent names and a malformed
	// utm_term=-kubecon-eu-. Trim once here so every consumer sees the same value.
	in.EventName = strings.TrimSpace(in.EventName)

	// Normalize Objective in place (trim + lowercase) so every consumer sees the
	// same value: objectiveParams keys are lowercase, so a padded/upper value like
	// " Traffic" would otherwise fail the lookup as "unknown" even though it is
	// valid, and a whitespace-only value would not be treated as empty (and so not
	// default to "traffic"). buildCampaignName also reads in.Objective, so normalize
	// before it is called.
	in.Objective = strings.ToLower(strings.TrimSpace(in.Objective))

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

	// Step 1: Account preflight (GET the ad-account object). This both verifies
	// access and fetches the account's ISO 4217 currency CODE — from which the
	// minor-unit offset used to encode the budget is DERIVED (see below; the
	// AdAccount node does not expose a currency_offset field). It runs BEFORE any
	// mutating call, so an unknown/undeterminable currency fails before a paid
	// resource exists.
	//
	// A genuine CALLER-context cancellation/deadline must short-circuit here —
	// otherwise, for inputs that go on to fail the geo checks, CreateCampaign would
	// return that geo-validation error and mask the fact that the caller cancelled.
	// Distinguish the caller ctx (ctx.Err() != nil) from the client's own
	// http.Client.Timeout, which surfaces as a DeadlineExceeded-wrapped error while
	// the caller ctx is still live.
	var acct accountPreflight
	preflightErr := c.doRequest(ctx, http.MethodGet, "/"+accountID+"?fields=name,account_status,currency", nil, &acct)
	if preflightErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("meta campaign aborted during account preflight: %w", ctx.Err())
		}
		steps = append(steps, fmt.Sprintf("Account preflight warning: %s", truncateErr(preflightErr, 300)))
	} else {
		// The preflight fetched account_status; a successful GET is not the same as an
		// ACTIVE account. If the account is in a known-inactive state, fail BEFORE any
		// mutating call rather than creating a paid campaign that Meta would reject at a
		// later step. A status of 0 (unreported) or any value not known to be bad is
		// allowed through — this blocks only definitively-disabled accounts.
		if reason, bad := inactiveAccountStatusLabels[acct.AccountStatus]; bad {
			return nil, fmt.Errorf("meta ad account %s is not active (account_status %d: %s); resolve the account status in Meta Ads Manager before creating campaigns", accountID, acct.AccountStatus, reason)
		}
		if acct.AccountStatus == metaAccountStatusActive {
			steps = append(steps, fmt.Sprintf("Account verified: %s (%s, active)", label, accountID))
		} else {
			steps = append(steps, fmt.Sprintf("Account verified: %s (%s)", label, accountID))
		}
	}

	// Resolve the currency offset used to convert the whole-currency-unit budget to
	// Meta minor units (NOT an FX conversion — the caller's amount is already in the
	// account's currency). Most currencies use 100; zero-decimal currencies
	// (JPY/KRW/CLP) use 1. Precedence: the ACCOUNT CURRENCY is authoritative — if
	// the preflight returns a recognized currency, its derived offset is used, and a
	// conflicting explicit AccountConfig.CurrencyOffset is REJECTED (a stale
	// override would mis-scale the budget). An explicit offset is only relied on as
	// a FALLBACK when the preflight fails or its currency isn't in the
	// supported-currency map. If neither yields a usable (positive) offset — the
	// currency is unknown/absent AND no explicit offset — fail HERE, before any
	// mutating call, rather than guessing 100, which would silently encode a
	// zero-decimal budget 100× too high (a warning after resource creation cannot
	// prevent that budget from being activated).
	offset := c.account.CurrencyOffset
	if offset == 0 {
		if preflightErr != nil {
			// Wrap with %w (not %s) so the underlying error chain is preserved and a
			// caller can errors.As it back to *APIError like other Graph failures — a
			// %s would flatten it to a string and break that unwrap.
			return nil, fmt.Errorf("meta: could not determine the account currency because the account preflight failed; set AccountConfig.CurrencyOffset explicitly (100 for most currencies, 1 for zero-decimal like JPY/KRW/CLP): %w", preflightErr)
		}
		derived, ok := currencyOffsetFor(acct.Currency)
		if !ok {
			return nil, fmt.Errorf("meta: account preflight returned an unsupported or missing currency code (got %q); it is not in the supported-currency map, so set AccountConfig.CurrencyOffset explicitly (100 for most currencies, 1 for zero-decimal like JPY/KRW/CLP) rather than assuming a default that could encode a zero-decimal budget 100x too high", acct.Currency)
		}
		offset = derived
	} else if preflightErr == nil {
		// An explicit override is set AND the preflight returned a currency. If that
		// currency is recognized and its true offset DIFFERS from the override,
		// reject rather than trust the override: a stale override (e.g. a persisted
		// CurrencyOffset:100 on an account whose currency is now JPY, true offset 1)
		// would silently encode the budget 100× wrong. The account's actual currency
		// is authoritative; only rely on the override when the preflight can't
		// identify the currency (unrecognized/absent code -> derived !ok).
		if derived, ok := currencyOffsetFor(acct.Currency); ok && derived != offset {
			return nil, fmt.Errorf("meta: AccountConfig.CurrencyOffset (%d) conflicts with the account's currency %q (correct offset %d) reported by the preflight; the account currency is authoritative — remove or correct the explicit offset to avoid encoding the budget with the wrong minor-unit scale", offset, acct.Currency, derived)
		}
	}

	// Convert whole account-currency units to Meta minor units and reject budgets
	// that round to zero minor units — all before any mutating call, so a
	// zero/invalid budget never creates a bad ad set.
	//
	// Guard against int64 overflow of the SCALED value before converting. This is
	// the ONLY budget-magnitude ceiling (there is no fixed major-unit cap): both
	// Budget and the offset are otherwise unbounded, so a genuinely huge budget — or
	// a bogus large explicit/preflight offset — could push the product past int64.
	// Converting an out-of-range float to int64 is implementation-defined, so
	// range-check the float product first rather than relying on the budgetMinor<1
	// check to catch a wrapped value. math.MaxInt64 is not exactly representable as a
	// float64, so compare against float64(math.MaxInt64) (which rounds up); a scaled
	// value at or above it (including +Inf from an absurd budget) is rejected as out
	// of range for a currency amount.
	scaled := math.Round(in.Budget * float64(offset))
	if scaled >= float64(math.MaxInt64) {
		return nil, fmt.Errorf("budget too large after applying currency offset %d: exceeds the representable minor-unit range", offset)
	}
	budgetMinor := int64(scaled)
	if budgetMinor < 1 {
		return nil, fmt.Errorf("budget too small: must be at least one minor currency unit (offset %d)", offset)
	}

	// Step 2: geo filtering + campaign creation.
	// If the caller supplied geo targets but NONE survive validation (all bogus or
	// sanctioned), fail rather than silently falling back to US and targeting a
	// country they didn't ask for. An empty input legitimately defaults to US.
	allGeo := validateGeoTargets(in.GeoTargets)
	// Surface geos that were supplied but dropped by validateGeoTargets (bogus/
	// non-ISO codes, or Meta-ineligible/sanctioned countries like IR/CU/KP/RU) as
	// an explicit step, so a caller who mixed eligible + ineligible codes isn't
	// left believing an excluded country is being targeted. This mirrors the
	// regulated-country (SG/TW/KR) step emitted below. Skip the note when the only
	// difference is the empty-input US fallback.
	var droppedGeos []string
	if len(in.GeoTargets) > 0 {
		kept := make(map[string]struct{}, len(allGeo))
		for _, g := range allGeo {
			kept[g] = struct{}{}
		}
		seenDropped := make(map[string]struct{})
		for _, g := range in.GeoTargets {
			up := strings.ToUpper(strings.TrimSpace(g))
			if up == "" {
				continue
			}
			if _, ok := kept[up]; ok {
				continue
			}
			if _, dup := seenDropped[up]; dup {
				continue
			}
			seenDropped[up] = struct{}{}
			droppedGeos = append(droppedGeos, up)
		}
		if len(droppedGeos) > 0 {
			steps = append(steps, fmt.Sprintf("Geo targets dropped (invalid code or not eligible for Meta ad targeting, e.g. sanctioned/excluded countries): %s", strings.Join(droppedGeos, ", ")))
		}
	}
	if len(in.GeoTargets) > 0 && len(allGeo) == 1 && allGeo[0] == "US" {
		// Only a real problem if the caller didn't actually ask for US: this means
		// every supplied geo was invalid or sanctioned and we fell back to US.
		askedUS := false
		for _, g := range in.GeoTargets {
			if strings.EqualFold(strings.TrimSpace(g), "US") {
				askedUS = true
				break
			}
		}
		if !askedUS {
			// NAME the dropped geos in the error (the "Geo targets dropped" step is
			// discarded when we return nil), so the caller learns exactly which codes
			// were invalid/ineligible rather than a generic message.
			return nil, fmt.Errorf("no usable geo targets: all supplied geos are invalid or ineligible for Meta ads targeting (dropped: %s) — refusing to silently fall back to US", strings.Join(droppedGeos, ", "))
		}
	}
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
		return nil, fmt.Errorf("meta campaign skipped: all selected geo targets (%s) are regulated and excluded from API targeting; supply at least one eligible (non-regulated) geo target", strings.Join(skippedGeos, ", "))
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
		// A 2xx with no id is a malformed success: Meta may have created a PAUSED
		// campaign whose id we couldn't read. Return a partial result carrying the
		// campaign NAME so an orphan is reconcilable by name (not discarded), with an
		// UNCONFIRMED note. NOTE: the campaign POST is not retry-safe in general —
		// Meta exposes no create idempotency key, so a lost/timed-out response can't
		// be distinguished from a not-created one; true retry-safe idempotency is
		// tracked in LFXV2-2665. This makes the malformed-success case reconcilable.
		steps = append(steps, "Campaign creation returned no campaign ID (malformed response); a PAUSED campaign may exist — verify by name in Meta Ads Manager")
		return &CampaignResult{
			Platform:     "meta-ads",
			CampaignName: campaignName,
			MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
			Steps:        steps,
		}, fmt.Errorf("meta campaign creation succeeded but returned no campaign ID (a PAUSED campaign %q may exist)", campaignName)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (%s, PAUSED)", campaignID, objectiveLabel(objective)))

	// Step 3: Ad set (budget, placements, and promoted object were validated up
	// front, before the campaign was created).
	adSetName := fmt.Sprintf("%s - %s", in.EventName, objectiveLabel(objective))

	// partialResult builds a *CampaignResult carrying the resources already created
	// (the PAUSED campaign, and the ad set once it exists) plus the steps so far.
	// It is returned ALONGSIDE the error at every downstream failure point after the
	// campaign POST succeeds, so an orphaned paid resource is identifiable by ID for
	// cleanup/reconcile without parsing the human-readable error string, and a caller
	// retry can reconcile instead of blindly re-creating. adSetID/adCount are captured
	// by reference so the result reflects whatever exists at the failure point.
	// Mirrors the twitter/reddit clients' partial-result helper.
	var adSetID string
	adCount := 0
	partialResult := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "meta-ads",
			CampaignName: campaignName,
			CampaignID:   campaignID,
			AdSetName:    adSetName,
			AdSetID:      adSetID,
			AdCount:      adCount,
			MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
			Steps:        steps,
		}
	}

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
		"start_time":        adSetStartTime(startDate, c.timeNow()),
		"end_time":          in.EndDate + "T23:59:59+0000",
	}

	if promotedObject != nil {
		adSetBody["promoted_object"] = promotedObject
	}

	if in.LifetimeBudget {
		adSetBody["lifetime_budget"] = budgetMinor
	} else {
		adSetBody["daily_budget"] = budgetMinor
	}

	var adSetResp createResponse
	if err := c.doRequest(ctx, http.MethodPost, "/"+accountID+"/adsets", adSetBody, &adSetResp); err != nil {
		// The campaign was already created (PAUSED). Return a partial result carrying
		// its id so the caller can identify/clean up the orphan without parsing the
		// error string; auto-deleting here would race a retry that reuses it.
		return partialResult(), fmt.Errorf("meta ad set creation failed (campaign %s created, PAUSED): %w", campaignID, err)
	}
	adSetID = adSetResp.ID
	if adSetID == "" {
		return partialResult(), fmt.Errorf("meta ad set creation succeeded but returned no ad set ID (campaign %s created, PAUSED)", campaignID)
	}
	budgetLabel := "daily"
	if in.LifetimeBudget {
		budgetLabel = "lifetime"
	}
	// Currency-neutral: Meta interprets the budget in the ad account's currency,
	// which may not be USD, so don't prefix with '$'.
	steps = append(steps, fmt.Sprintf("Ad set created: %s (%.2f %s budget, geo: %s)", adSetID, in.Budget, budgetLabel, strings.Join(geoCountries, ", ")))

	// Step 4: creative + ad per variant (per-variant failures are non-fatal).
	for i, variant := range validVariants {
		utmURL := buildUTMURL(in, i)

		adID, creativeID, verr := c.createVariantAd(ctx, in, variant, adSetID, utmURL, i)
		if verr != nil {
			// A cancelled or deadlined CALLER context is fatal: continuing would let
			// us report a "successful" campaign after the caller's context died. Key
			// the decision off the caller ctx directly (ctx.Err()), NOT errors.Is on
			// the returned error: the client's own http.Client.Timeout also surfaces
			// as a DeadlineExceeded-wrapped url error, but with a still-live caller
			// ctx that per-creative timeout is an ordinary API failure and must stay
			// non-fatal (skip + continue), like any other per-creative error.
			if ctx.Err() != nil {
				// If the creative was created before the ad call was cut short, surface
				// its id in the fatal error too — otherwise this known orphaned creative
				// is lost (the non-fatal path below already reports it).
				if creativeID != "" {
					return partialResult(), fmt.Errorf("meta campaign aborted while creating ad %d (campaign %s created, PAUSED; orphaned creative: %s): %w", i+1, campaignID, creativeID, ctx.Err())
				}
				return partialResult(), fmt.Errorf("meta campaign aborted while creating ad %d (campaign %s created, PAUSED): %w", i+1, campaignID, ctx.Err())
			}
			// If the creative was created before the ad failed, surface its id so the
			// orphaned creative is visible (can be cleaned up / reused) rather than
			// silently discarded.
			if creativeID != "" {
				steps = append(steps, fmt.Sprintf("Ad %d failed: %s (orphaned creative: %s)", i+1, truncateErr(verr, 300), creativeID))
			} else {
				steps = append(steps, fmt.Sprintf("Ad %d failed: %s", i+1, truncateErr(verr, 300)))
			}
			continue
		}
		adCount++
		steps = append(steps, fmt.Sprintf("Ad %d created: %s (creative: %s) → %s", i+1, adID, creativeID, utmURL))
	}

	if adCount == 0 && len(in.Variants) > 0 {
		steps = append(steps, "No ads could be created — create them manually in Meta Ads Manager")
	}

	// Success: partialResult() now carries the fully-created campaign, ad set, and
	// ad count (same fields as a bespoke literal); reuse it so success and partial
	// failure return an identically-shaped result.
	return partialResult(), nil
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
		// The creative was already created; return its id alongside the error so
		// the (non-fatal) caller can record the orphaned creative rather than
		// silently discarding it.
		return "", creativeResp.ID, err
	}
	if adResp.ID == "" {
		return "", creativeResp.ID, fmt.Errorf("ad creation returned no ID")
	}
	return adResp.ID, creativeResp.ID, nil
}

func objectiveKeys() []string {
	// The objectives CreateCampaign accepts. All five are supported; 'leads' runs
	// as a website-leads campaign (LINK_CLICKS to the registration URL).
	return []string{"awareness", "traffic", "engagement", "leads", "conversions"}
}
