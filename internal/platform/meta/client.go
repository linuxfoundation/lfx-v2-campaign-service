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
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
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
	//
	// It MUST comfortably exceed doRequest's worst-case retry budget: with the
	// default client a single doRequest can span up to (retryMax+1)=4 attempts,
	// each bounded by DefaultRequestTimeout (30s), plus up to retryMax=3
	// Retry-After waits each capped at maxRetryWait (60s) — i.e. roughly
	// 4×30s + 3×60s ≈ 5 minutes. If the buffer were only ~5 minutes, the ad-set
	// POST on the LAST retry could carry a start_time that has already slipped
	// into the past (or, at a day boundary, onto the wrong day), which Meta
	// rejects. 10 minutes clears that ~5-minute worst case with headroom for
	// scheduling/network latency before the request actually reaches Meta.
	adSetStartBuffer = 10 * time.Minute
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

// noFollow is the CheckRedirect policy for every client this package uses: it
// returns http.ErrUseLastResponse so the client does NOT follow redirects and
// hands the 3xx response back to the request layer, where a non-2xx status is
// surfaced as an error. The Graph API returns JSON directly and never legitimately
// 3xx-redirects these calls; not following keeps outcome classification sound — a
// redirect can't carry an already-sent mutating POST to a different target and be
// misclassified. It is shared by the built-in client and the caller-supplied-
// client enforcement in NewClient. Mirrors the reddit/linkedin/googleads clients.
func noFollow(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// WithHTTPClient overrides the HTTP client (useful for tests / timeouts). Redirect
// following is force-disabled on whatever client ends up in use (see NewClient),
// so an injected client cannot reintroduce redirect following.
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
		httpClient:     &http.Client{Timeout: DefaultRequestTimeout, CheckRedirect: noFollow},
		baseURL:        DefaultBaseURL,
		adsManagerURL:  DefaultAdsManagerURL,
		timeNow:        time.Now,
		retryBaseDelay: retryBaseDelay,
	}
	for _, o := range opts {
		o(c)
	}
	// Enforce the no-follow redirect policy UNCONDITIONALLY on whatever client ended
	// up on c.httpClient — INCLUDING one supplied via WithHTTPClient, which replaces
	// the default above. Following a redirect would carry an already-sent mutating
	// POST to a different target and muddy outcome classification, so no-follow is a
	// correctness requirement, not a default.
	//
	// Build a FRESH *http.Client rather than value-copying the caller's: an
	// http.Client must not be copied after first use (a value copy duplicates its
	// internal mutex while sharing the request-cancellation map, so concurrent use
	// of the caller's client and our copy can race). We carry over only the exported,
	// reusable fields (Transport, Jar, Timeout) — the shareable connection pool /
	// cookie jar / deadline — and set our own CheckRedirect. The caller's client is
	// never mutated and is safe to keep using elsewhere.
	if c.httpClient != nil {
		c.httpClient = &http.Client{
			Transport:     c.httpClient.Transport,
			CheckRedirect: noFollow,
			Jar:           c.httpClient.Jar,
			Timeout:       c.httpClient.Timeout,
		}
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

// findCampaignByName looks up an existing campaign in the ad account by exact
// name match. Meta enforces no name-uniqueness constraint and exposes no create
// idempotency key (see CreateCampaign's doc comment), so this is OUR safeguard —
// mirroring the isDuplicateCampaignNameErr reconcile pattern used by the
// googleads/microsoft clients — run BEFORE the create call rather than reactively
// after a duplicate-name rejection, since Meta would silently create a second
// paid campaign with the same name instead of rejecting it.
//
// Returns ("", nil) when no campaign with that name exists. A non-nil error means
// the lookup itself failed — the caller cannot tell absence from a failed check,
// so it must NOT proceed to create as if the name were confirmed free.
//
// NOTE: Name matching alone does NOT guarantee idempotency. buildCampaignName
// is deterministic but does not include brief identity and collapses geos
// broadly (e.g., US and CA both become NA), so distinct briefs can generate
// the same name. A full solution requires embedding a stable per-brief
// idempotency discriminator in the campaign (tracked in LFXV2-2665). For now,
// we do basic property validation (status=PAUSED, objective match) to reduce
// the chance of reusing an unrelated campaign, but a name collision across
// distinct briefs remains possible and is accepted as a known limitation.
//
// REACHABILITY — which retries actually reach this lookup, and which do not:
//
//	REACHED: a retry after a CLEAN dispatch failure. The claim is released, so the
//	orchestrator re-dispatches, CreateCampaign runs again, and the lookup finds whatever
//	the failed attempt may nevertheless have created. This is the path that used to
//	duplicate paid campaigns, and the one this lookup closes.
//
//	NOT REACHED: a retry that finds a RETAINED PARTIAL orphan (a row carrying a
//	PlatformCampaignID or a Result reconcile blob). ProcessJob reports those as
//	"reconciliation required" and never calls the dispatcher — deliberately, since a
//	human may already be reconciling the row upstream. Making Meta's name-idempotent
//	create re-dispatch such a row is a change to the SHARED retry path (it must not
//	re-dispatch a platform whose create is not idempotent), so it is its own piece of
//	work, not part of this lookup. Tracked under LFXV2-2665.
//
// errLookupAmbiguous marks every lookup outcome that leaves ABSENCE UNCONFIRMED,
// which createOutcomeAmbiguous then classifies like a 5xx rather than a clean
// failure. That covers a malformed-but-2xx body (missing data field, a matched row
// with no usable id or a non-numeric one) AND an enumeration we could not finish
// (a next link with no cursor, or the page cap reached with pages still pending):
// in both cases Meta DID respond and unexamined matches may remain.
//
// The classification is what makes the lookup worth having. A clean failure lets
// the dispatcher release the claim, and the retry re-POSTs the same deterministic
// name into an account where Meta enforces no uniqueness — a duplicate PAID
// campaign, the exact defect this lookup exists to prevent. Only a definite answer
// ("this name is absent", or "it exists but is unusable for a stated reason") may
// be a clean failure.
var errLookupAmbiguous = errors.New("meta lookup outcome ambiguous; cannot confirm absence")

// errLookupConflict marks the exact opposite outcome: the lookup SUCCEEDED and found the
// name occupied by a resource this create cannot adopt (a match that is not PAUSED, or
// whose objective differs from the requested one). Absence is not unconfirmed here —
// PRESENCE is confirmed, with a stated reason. Nothing was created, nothing can be
// adopted, and a retry re-reads the very same conflict rather than POSTing a duplicate, so
// this is a CLEAN failure. Wrapping it in errLookupAmbiguous would tell an operator to go
// verify in Ads Manager something the error already states, and would leave the dispatcher
// holding a partial for a create that provably never happened.
var errLookupConflict = errors.New("meta lookup found the name already in use by a resource that cannot be reused")

func (c *Client) findCampaignByName(ctx context.Context, accountID, name, expectedObjective string) (string, error) {
	filtering, err := json.Marshal([]map[string]string{{"field": "name", "operator": "EQUAL", "value": name}})
	if err != nil {
		return "", fmt.Errorf("meta campaign lookup: encoding name filter: %w", err)
	}
	basePath := "/" + accountID + "/campaigns?fields=id,status,objective&filtering=" + url.QueryEscape(string(filtering))

	// Follow pagination like listAdIDs does, so an empty first page with a next link
	// doesn't trick us into missing a match on a later page. Meta's Graph API can
	// return an empty first page under filtering (visibility/privacy constraints)
	// but still have results on subsequent pages.
	// Accumulate all matches across all pages before deciding if the lookup is
	// ambiguous or returns a unique campaign ID.
	var allMatches []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Objective string `json:"objective"`
	}
	after := ""
	page := 0
	for page < adDiscoveryMaxPages {
		path := basePath
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			// Data is a pointer slice so an absent `data` field (malformed 2xx) is
			// distinguishable from a present-but-empty `{"data":[]}` — see listAdIDs'
			// identical reasoning. A malformed body must NOT be read as "no match".
			Data *[]struct {
				ID        string `json:"id"`
				Status    string `json:"status"`
				Objective string `json:"objective"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return "", err
		}
		if resp.Data == nil {
			return "", fmt.Errorf("meta campaign lookup for %q returned a 2xx response with no data field; cannot confirm absence: %w", name, errLookupAmbiguous)
		}
		if len(*resp.Data) > 0 {
			allMatches = append(allMatches, *resp.Data...)
		}
		// Empty page or final page; check for more pages.
		if resp.Paging.Next == "" {
			break // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			// Ambiguous, not a clean failure: Meta says more pages exist and we cannot
			// read them, so unexamined matches may remain. Reporting a clean failure
			// releases the claim and lets a retry re-POST the same name.
			return "", fmt.Errorf("meta campaign lookup for %q has more pages but no cursor; cannot guarantee the campaign name is absent: %w", name, errLookupAmbiguous)
		}
		page++
	}
	if page >= adDiscoveryMaxPages && after != "" {
		// Reached page cap with pagination still pending: fail closed.
		return "", fmt.Errorf("meta campaign lookup for %q exceeded %d pages; too many campaigns with that name to enumerate: %w", name, adDiscoveryMaxPages, errLookupAmbiguous)
	}

	// No matches found.
	if len(allMatches) == 0 {
		return "", nil
	}

	// Multiple matches mean name is not a unique identifier — we can't
	// tell which is the intended campaign. Fail closed rather than
	// silently pick the first one, which could attach to an unrelated
	// campaign.
	if len(allMatches) > 1 {
		return "", fmt.Errorf("meta campaign lookup for %q matched %d existing campaigns; cannot disambiguate which is intended: %w", name, len(allMatches), errLookupAmbiguous)
	}

	// Single match: validate status=PAUSED and objective matches, then return the campaign ID.
	id := strings.TrimSpace(allMatches[0].ID)
	if id == "" {
		return "", fmt.Errorf("meta campaign lookup for %q matched an existing campaign with no usable id: %w", name, errLookupAmbiguous)
	}
	// Non-empty is not the same as safe to interpolate. The caller splices this id
	// straight into "/{campaignID}/adsets", so it gets the same numericIDRE gate every
	// other id-interpolating path in this client applies (UpdateCampaignStatus,
	// createAdSet). Ambiguous rather than a clean failure: a campaign with this name DOES
	// exist upstream — we just cannot address it — and reporting "nothing here" would let
	// a retry create a duplicate.
	if !numericIDRE.MatchString(id) {
		return "", fmt.Errorf("meta campaign lookup for %q matched an existing campaign with a non-numeric id %q; cannot use it: %w", name, id, errLookupAmbiguous)
	}
	// Validate the matched campaign is in PAUSED state. A non-PAUSED campaign is a
	// definite conflict (name already taken by a live campaign), not an ambiguous
	// lookup — a name match with ACTIVE status means the name IS taken and cannot
	// be reused. Return a definite error (not errLookupAmbiguous) so
	// createOutcomeAmbiguous treats it as a clean failure, not UNCONFIRMED.
	status := strings.TrimSpace(allMatches[0].Status)
	if status != "PAUSED" {
		return "", fmt.Errorf("meta campaign lookup for %q matched an existing campaign with status=%q (expected PAUSED); campaign name is already in use and cannot be reused: %w", name, status, errLookupConflict)
	}
	// Validate the matched campaign's objective matches the requested one. A
	// mismatch is a definite conflict (name already taken by a differently-
	// configured campaign), not an ambiguous lookup. Return a definite error (not
	// errLookupAmbiguous) so createOutcomeAmbiguous treats it as a clean failure.
	objective := strings.TrimSpace(allMatches[0].Objective)
	if objective != expectedObjective {
		return "", fmt.Errorf("meta campaign lookup for %q matched an existing campaign with objective=%q (expected %q); campaign name is already in use with a different objective and cannot be reused: %w", name, objective, expectedObjective, errLookupConflict)
	}
	return id, nil
}

// findAdSetByName mirrors findCampaignByName, scoped to the ad sets under a
// single already-resolved campaignID (the campaign's own name match already
// disambiguates the account, so no separate accountID filter is needed).
func (c *Client) findAdSetByName(ctx context.Context, campaignID, name string) (string, error) {
	filtering, err := json.Marshal([]map[string]string{{"field": "name", "operator": "EQUAL", "value": name}})
	if err != nil {
		return "", fmt.Errorf("meta ad set lookup: encoding name filter: %w", err)
	}
	basePath := "/" + campaignID + "/adsets?fields=id,status&filtering=" + url.QueryEscape(string(filtering))

	// Follow pagination like listAdIDs does, so an empty first page with a next link
	// doesn't trick us into missing a match on a later page.
	// Accumulate all matches across all pages before deciding if the lookup is
	// ambiguous or returns a unique ad set ID.
	var allMatches []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	after := ""
	page := 0
	for page < adDiscoveryMaxPages {
		path := basePath
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			Data *[]struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return "", err
		}
		if resp.Data == nil {
			return "", fmt.Errorf("meta ad set lookup for %q returned a 2xx response with no data field; cannot confirm absence: %w", name, errLookupAmbiguous)
		}
		if len(*resp.Data) > 0 {
			allMatches = append(allMatches, *resp.Data...)
		}
		// Empty page or final page; check for more pages.
		if resp.Paging.Next == "" {
			break // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			// Same reasoning as findCampaignByName: pagination we cannot follow leaves
			// absence unconfirmed, so this is ambiguous, not a clean failure.
			return "", fmt.Errorf("meta ad set lookup for %q has more pages but no cursor; cannot guarantee the ad set name is absent: %w", name, errLookupAmbiguous)
		}
		page++
	}
	if page >= adDiscoveryMaxPages && after != "" {
		// Reached page cap with pagination still pending: fail closed.
		return "", fmt.Errorf("meta ad set lookup for %q exceeded %d pages; too many ad sets with that name to enumerate: %w", name, adDiscoveryMaxPages, errLookupAmbiguous)
	}

	// No matches found.
	if len(allMatches) == 0 {
		return "", nil
	}

	// Multiple matches mean name is not a unique identifier — we can't
	// tell which is the intended ad set. Fail closed rather than
	// silently pick the first one, which could attach newly created ads
	// to an unrelated ad set.
	if len(allMatches) > 1 {
		return "", fmt.Errorf("meta ad set lookup for %q matched %d existing ad sets; cannot disambiguate which is intended: %w", name, len(allMatches), errLookupAmbiguous)
	}

	// Single match: validate status=PAUSED and return the ad set ID.
	id := strings.TrimSpace(allMatches[0].ID)
	if id == "" {
		return "", fmt.Errorf("meta ad set lookup for %q matched an existing ad set with no usable id: %w", name, errLookupAmbiguous)
	}
	// Same gate as the campaign lookup above: this id is interpolated into ad-create
	// paths, and an ad set with this name existing but being unaddressable is ambiguous,
	// not absent.
	if !numericIDRE.MatchString(id) {
		return "", fmt.Errorf("meta ad set lookup for %q matched an existing ad set with a non-numeric id %q; cannot use it: %w", name, id, errLookupAmbiguous)
	}
	// Validate the matched ad set is in PAUSED state. A non-PAUSED ad set is a
	// definite conflict (name already taken by a live ad set), not an ambiguous
	// lookup — a name match with ACTIVE status means the name IS taken and cannot
	// be reused. Return a definite error (not errLookupAmbiguous) so
	// createOutcomeAmbiguous treats it as a clean failure, not UNCONFIRMED.
	status := strings.TrimSpace(allMatches[0].Status)
	if status != "PAUSED" {
		return "", fmt.Errorf("meta ad set lookup for %q matched an existing ad set with status=%q (expected PAUSED); ad set name is already in use and cannot be reused: %w", name, status, errLookupConflict)
	}
	return id, nil
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
	// EnvelopeUnreadable marks a non-2xx whose Graph error envelope could NOT be read —
	// the body was oversized or the read failed, so Code is absent because we never got
	// it, not because Meta omitted it. The two are indistinguishable from the fields
	// above, and the difference decides a create's fate: Meta reports rate limiting as an
	// HTTP 400 carrying a rate-limit Code far more often than as a 429, so a 400 with no
	// Code reads as a clean semantic rejection. Classify an unread envelope that way and
	// a shed create — which Meta may already have committed — is reported as definitely
	// failed, the claim is released, and the retry duplicates a PAID campaign.
	// createOutcomeAmbiguous treats this as ambiguous for exactly that reason.
	EnvelopeUnreadable bool
}

func (e *APIError) Error() string {
	// Mirror the TS behavior of not leaking full bodies to callers while still
	// surfacing status; include the parsed message when available, plus the Graph
	// diagnostic fields (type/code/fbtrace_id) when present — fbtrace_id in
	// particular is essential when opening a Meta support ticket.
	var b strings.Builder
	if e.Message != "" {
		fmt.Fprintf(&b, "meta API %s %s failed (%d): %s", e.Method, e.Path, e.StatusCode, e.Message)
	} else {
		fmt.Fprintf(&b, "meta API %s %s failed (%d) with no error details in the response body", e.Method, e.Path, e.StatusCode)
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

// transportError wraps a failure of the HTTP round-trip itself (httpClient.Do)
// that happened AFTER the request was plausibly sent (mid-flight timeout,
// unexpected EOF, connection reset): the server may or may not have processed
// it, so the outcome is AMBIGUOUS. This is distinct from a pre-send failure
// (access token missing, body encode, request build, or a pre-connect dial
// error — see isPreSendDialError), where the request never reached Meta and a
// mutation definitely did not happen. Callers use it to decide whether a failed
// create is "may exist" (ambiguous) vs "not created". Mirrors the reddit client.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("meta API %s %s: %v", e.Method, e.Path, e.Err)
}
func (e *transportError) Unwrap() error { return e.Err }

// isPreSendDialError reports whether a httpClient.Do error clearly happened
// BEFORE any request bytes could have reached Meta (DNS resolution failure,
// connection refused, or no route/network unreachable). Such a failure means the
// request was NOT sent, so it must NOT be treated as an ambiguous "may exist"
// transportError. A failure AFTER a connection is established (mid-flight
// timeout, unexpected EOF) is genuinely ambiguous and IS wrapped as
// transportError. Mirrors the reddit client.
func isPreSendDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	return false
}

// createOutcomeAmbiguous reports whether a failed mutating request MAY have been
// applied by Meta despite the error — i.e. the request plausibly reached the
// server and its outcome is unknowable. It is the single source of truth shared
// by the campaign and ad/creative create paths so they classify identically:
//   - transportError: the round-trip failed AFTER a connection was established
//     (a pre-connect dial error is NOT wrapped as transportError, so it never
//     reaches here), so the request may have been received;
//   - *APIError with a 5xx status: Meta received it and may have committed the
//     mutation before erroring.
//   - *APIError with a 3xx status: redirect following is force-disabled (see
//     noFollow), so a 3xx is surfaced here rather than followed. A 3xx on a
//     mutating request is NOT a definite rejection — Meta may have committed the
//     create and then returned a redirect — so it is ambiguous like a 5xx.
//
// A 429 that survives the retry budget is ambiguous too: it is a throttle, not a
// rejection, so it establishes neither "not applied" nor "the name is absent".
//
// A definite 4xx (Meta rejected it), or any pre-send failure (token missing,
// body encode/build, a pre-connect dial error), means NOT applied → returns
// false so the caller returns a clean (nil, err) / "failed" rather than "may
// exist". Mirrors the reddit client's createOutcomeAmbiguous.
func createOutcomeAmbiguous(err error) bool {
	// A malformed-but-2xx lookup response (missing data field, or a matched row
	// with no usable id) means Meta DID respond — it just can't be read as a
	// confirmed absence — so it is ambiguous exactly like a 5xx, not a clean
	// failure. See findCampaignByName/findAdSetByName.
	if errors.Is(err, errLookupAmbiguous) {
		return true
	}
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	// A 5xx may follow a committed create.
	if ae.StatusCode >= 500 {
		return true
	}
	// 429 is the one 4xx that is NOT a semantic rejection. Reaching here means the
	// bounded retry in doRequest was EXHAUSTED (or aborted on an over-cap
	// Retry-After), so this is a throttle we never got past — and a throttle says
	// nothing about what happened to the request. Two distinct callers need that:
	//   - a mutating call may have been shed AFTER Meta committed the node, so a
	//     clean-failure classification invites a blind retry that duplicates a PAID
	//     campaign;
	//   - the name LOOKUP exists to confirm ABSENCE, and a throttled lookup confirms
	//     nothing, so the caller must be told to verify in Ads Manager rather than
	//     be handed a bare failure.
	// Unlike the 3xx case below this is deliberately NOT gated on the method: the
	// 3xx gate asks "could this have created something?", while 429 also has to
	// answer "did we establish absence?", and a GET cannot answer that either.
	// The HTTP status alone does not identify a Meta throttle. Meta reports rate
	// limiting as an HTTP 429 OR, commonly, as an HTTP 400 carrying a Graph error
	// envelope with a known rate-limit code — doRequest already treats both as
	// retryable for exactly that reason, and preserves the code on the APIError it
	// finally returns. Recognising only 429 here would classify an exhausted
	// HTTP-400 throttle as a clean rejection, which is the more common shape of the
	// two on the Marketing API, so the whole guard would miss its main case.
	if ae.StatusCode == http.StatusTooManyRequests || graphRateLimitCodes[ae.Code] {
		return true
	}
	// The rate-limit-code check above can only fire on an envelope we actually read. When
	// the body was oversized or unreadable there is no Code to test, and the absence is
	// evidence of nothing: the most common throttle shape on the Marketing API is a 400
	// whose rate-limit code lives in that very body. Treating it as a clean rejection is
	// the same duplicate-a-paid-campaign failure the check above exists to prevent, just
	// reached by a different route — so an unread envelope is ambiguous, not clean.
	if ae.EnvelopeUnreadable {
		return true
	}
	// A 3xx on a MUTATING request reached a responder and may have committed a
	// resource before redirecting — UNCONFIRMED. A 3xx on a GET is not a create, so
	// it stays non-ambiguous. Gating on the method (rather than treating every 3xx
	// as ambiguous) keeps this helper's contract correct for any caller, not just
	// the create path — and makes it genuinely identical to the reddit client.
	return ae.StatusCode >= 300 && ae.StatusCode < 400 && isMutatingMethod(ae.Method)
}

// ErrCampaignNotServable marks an ACTIVATE refused BEFORE any mutating call because the
// campaign cannot serve (e.g. its ad set has zero ads). It is a local/state condition, not a
// platform failure — the dispatcher maps it to a client 409, not a 503. Exposed as a sentinel
// so the dispatcher can classify it across the package boundary via IsNotServable. Mirrors the
// LinkedIn client's sentinel of the same name.
var ErrCampaignNotServable = errors.New("campaign cannot be made servable")

// IsNotServable reports whether err is (or wraps) ErrCampaignNotServable — an activate refused
// up front because the campaign has nothing to serve.
func IsNotServable(err error) bool { return errors.Is(err, ErrCampaignNotServable) }

// IsOutcomeUnconfirmed reports whether a mutating-request error (e.g. from
// UpdateCampaignStatus) leaves the outcome UNKNOWABLE — the request may have been applied by
// Meta even though it errored (a transportError, a 5xx, or a 3xx on a mutating method). A
// definite 4xx or a proven pre-send failure returns false. Exposes the same classifier the
// create paths use so a toggle caller can distinguish "may already reflect the change" from
// "definitely not applied". It also honors any error reporting Unconfirmed() bool — a
// partialCascadeError (campaign applied, a child then failed) is partially applied and must be
// treated as unconfirmed even if its underlying child error is a definite 4xx. Mirrors
// reddit.IsOutcomeUnconfirmed.
func IsOutcomeUnconfirmed(err error) bool {
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		return true
	}
	return createOutcomeAmbiguous(err)
}

// Campaign run states for UpdateCampaignStatus (Meta's Campaign.status enum values).
const (
	StatusActive = "ACTIVE"
	StatusPaused = "PAUSED"

	// adDiscoveryPageSize is the per-page limit for GET /{adSetID}/ads when discovering ads
	// to cascade a status change to. A broker ad set holds only a handful of ads, so one page
	// almost always suffices; the value is a comfortable upper bound.
	adDiscoveryPageSize = 100
	// adDiscoveryMaxPages bounds the paging loop as a runaway guard (a pathological ad set or
	// a paging cursor that never terminates can't spin forever). 100 pages × 100 ads is far
	// beyond any real broker-created ad set.
	adDiscoveryMaxPages = 100
)

// UpdateCampaignStatus sets an existing campaign's status to ACTIVE or PAUSED. Meta's Graph
// API updates a node via POST to the node id itself with the changed field, so this POSTs
// /{campaignID} with {"status": ...} (the same status enum the create path sets). campaignID
// is validated numeric (numericIDRE) before interpolation to prevent path/query injection.
func (c *Client) UpdateCampaignStatus(ctx context.Context, campaignID, status string) error {
	campaignID = strings.TrimSpace(campaignID)
	if campaignID == "" {
		return fmt.Errorf("meta: campaign id is required")
	}
	if !numericIDRE.MatchString(campaignID) {
		return fmt.Errorf("meta: invalid campaign id %q: must be numeric", campaignID)
	}
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("meta: status must be %q or %q, got %q", StatusActive, StatusPaused, status)
	}
	if err := c.doRequest(ctx, http.MethodPost, "/"+campaignID, map[string]any{"status": status}, nil); err != nil {
		return fmt.Errorf("meta: update campaign %s status to %s: %w", campaignID, status, err)
	}
	return nil
}

// UpdateCampaignAndChildrenStatus sets status on the campaign and, when an ad set id is
// supplied, on the ad set AND each ad under it — the platform side of the campaign status
// toggle. CreateCampaign PAUSES the campaign, ad set, and every ad, so toggling only the
// campaign to ACTIVE would leave the ad set/ads PAUSED and the campaign would not serve.
//
// The ads are DISCOVERED via GET /{adSetID}/ads and each is POSTed to ACTIVE/PAUSED. Since
// LFXV2-3295 the campaign result ALSO records the created ad ids (CampaignResult.Ads), so this
// discovery is now a deliberate choice rather than a necessity: the persisted list names only
// the ads THIS service created at dispatch, while the live enumeration also picks up ads added
// to the ad set afterwards — and an ad left serving because the toggle never knew about it is
// exactly the partial activation this cascade exists to prevent. Discovery is also self-healing
// against a result blob written by an older version, which carries no Ads at all. Meta gates a
// child's serving by its parent's status ("all the objects below it automatically inherit"
// a paused/archived parent), so ordering is STATUS-DEPENDENT to avoid a partial activation
// leaving paid delivery running:
//   - ACTIVATE: the ad-set-and-ads step FIRST (the ad set, THEN each discovered ad), and the
//     campaign flipped ACTIVE LAST — the still-paused campaign gates every child until that
//     final flip, so a failure BEFORE any child mutation leaves NOTHING serving (a clean,
//     definite failure). But once the FIRST child mutation may have landed (the ad set POST, or
//     a later ad), the outcome is Unconfirmed even though the paused campaign still prevents
//     serving — a reconciler cannot assume the change was rolled back. classifyCascadeErr
//     encodes exactly this: mutatedBefore || ambiguous → partial/Unconfirmed.
//   - PAUSE: campaign FIRST (delivery stops at the gate immediately), then the ad set, then ads.
//
// An empty adSetID toggles the campaign alone — but this is allowed only on PAUSE (a degraded
// create can still be paused); ACTIVATE with no ad set id is REJECTED, since a campaign with no
// ad set cannot serve.
func (c *Client) UpdateCampaignAndChildrenStatus(ctx context.Context, campaignID, adSetID, status string) error {
	if status == StatusActive && strings.TrimSpace(adSetID) == "" {
		return fmt.Errorf("meta: cannot activate campaign %s: no ad set id is known, so the tree cannot be made servable", campaignID)
	}
	// Validate ids BEFORE any HTTP (pre-flight): a malformed persisted id is a definite,
	// retry-won't-help failure, so fail cleanly with NOTHING applied.
	campaignID = strings.TrimSpace(campaignID)
	if !numericIDRE.MatchString(campaignID) {
		return fmt.Errorf("meta: invalid campaign id %q: must be numeric", campaignID)
	}
	adSetID = strings.TrimSpace(adSetID)
	if adSetID != "" && !numericIDRE.MatchString(adSetID) {
		return fmt.Errorf("meta: invalid ad set id %q: must be numeric", adSetID)
	}
	if status == StatusActive {
		// Children first (still gated by the paused campaign), campaign last.
		if err := c.updateAdSetAndAds(ctx, adSetID, status, false); err != nil {
			return err
		}
		if err := c.UpdateCampaignStatus(ctx, campaignID, status); err != nil {
			return &partialCascadeError{stage: "campaign activate", err: err}
		}
		return nil
	}
	// PAUSE: campaign gate first (stops delivery now), then the children.
	if err := c.UpdateCampaignStatus(ctx, campaignID, status); err != nil {
		return err
	}
	return c.updateAdSetAndAds(ctx, adSetID, status, true)
}

// updateAdSetAndAds POSTs the status to the ad set and each discovered ad. mutatedBefore says
// whether an upstream change has already committed (the PAUSE path flips the campaign first):
// when false (the ACTIVATE path, before the campaign flip), a DEFINITE 4xx or a discovery
// failure is a CLEAN error (nothing serving yet); an ambiguous failure (5xx/transport) is
// Unconfirmed. When true, any failure is Unconfirmed. An empty adSetID is a no-op.
func (c *Client) updateAdSetAndAds(ctx context.Context, adSetID, status string, mutatedBefore bool) error {
	if adSetID == "" {
		return nil
	}
	activating := !mutatedBefore && status == StatusActive
	// DISCOVER-FIRST: list the ads BEFORE mutating anything. Discovery is a GET (no side
	// effect), so on the activate path (mutatedBefore==false) a discovery failure or the
	// zero-ads guard below is still a CLEAN, nothing-mutated error — matching the LinkedIn
	// sibling (discover creatives, refuse zero before touching the campaign). The previous
	// order (ad set POST first, then discover) turned a legitimately-degraded zero-ads campaign
	// into a non-converging 503 loop: it re-POSTed the ad set ACTIVE and then returned an
	// Unconfirmed partial every retry. Discovering first lets zero ads return a deterministic
	// not-servable error the dispatcher maps to 409 ("reprovision"), and re-POSTs nothing.
	adIDs, err := c.listAdIDs(ctx, adSetID)
	if err != nil {
		// Discovery is a READ that runs before any mutation on the activate path, so it is NOT
		// classified through createOutcomeAmbiguous (which treats every 5xx/transport as
		// ambiguous — correct only for a MUTATING call that may have committed). A pre-mutation
		// read failure applied nothing, so it is CLEAN; it is only a partial (Unconfirmed) when a
		// prior mutation already landed (mutatedBefore — e.g. the PAUSE path flipped the campaign
		// first). Mirrors the LinkedIn finder path exactly.
		if mutatedBefore {
			return &partialCascadeError{stage: "ad discovery", err: err}
		}
		// Render the cause with %v, NOT %w: a pre-mutation READ failure is CLEAN, but the
		// underlying error is often a transportError/5xx that createOutcomeAmbiguous (which
		// unwraps via errors.As) would treat as ambiguous → IsOutcomeUnconfirmed → a spurious
		// 503. Stringizing the cause breaks the unwrap chain so this stays a clean, definite
		// failure. (The mutatedBefore branch above intentionally KEEPS it ambiguous/partial.)
		return fmt.Errorf("meta: ad discovery for ad set %s failed (nothing was mutated): %v", adSetID, err)
	}
	// On ACTIVATE, a tree with ZERO ads can never serve — Meta creation treats per-variant ad
	// failures as non-fatal, so a degraded broker campaign can legitimately have an ad set but
	// no ads. Refuse BEFORE any mutation so this is a clean not-servable error (nothing changed
	// upstream → the dispatcher returns a deterministic 409, not a transient 503). PAUSE with
	// zero ads is fine (nothing to pause) and passes through.
	if activating && len(adIDs) == 0 {
		return fmt.Errorf("%w: meta ad set %s has no ads, so the campaign cannot serve", ErrCampaignNotServable, adSetID)
	}
	// Mutate BOTTOM-UP: ads first, then the ad set. Every child stays gated by the still-paused
	// campaign on the activate path, so the ordering contract ("children before the campaign
	// flip") holds. Once the FIRST ad POST may have committed, subsequent failures are partial
	// applications (mutatedBefore=true).
	for _, adID := range adIDs {
		if err := c.doRequest(ctx, http.MethodPost, "/"+adID, map[string]any{"status": status}, nil); err != nil {
			return classifyCascadeErr("ad", err, mutatedBefore)
		}
		mutatedBefore = true
	}
	if err := c.doRequest(ctx, http.MethodPost, "/"+adSetID, map[string]any{"status": status}, nil); err != nil {
		return classifyCascadeErr("ad set", err, mutatedBefore)
	}
	return nil
}

// classifyCascadeErr decides whether a cascade-step error is Unconfirmed. Once an upstream
// change may have committed (mutatedBefore, or an ambiguous outcome), it is a
// partialCascadeError (Unconfirmed). Otherwise — the activate path before the campaign flip,
// with a DEFINITE failure (a 4xx / clean rejection) — nothing is serving yet, so it is a clean
// error the caller may treat as "not applied".
func classifyCascadeErr(stage string, err error, mutatedBefore bool) error {
	if mutatedBefore || createOutcomeAmbiguous(err) {
		return &partialCascadeError{stage: stage, err: err}
	}
	return fmt.Errorf("meta: %s update failed: %w", stage, err)
}

// listAdIDs discovers the ad ids under an ad set via GET /{adSetID}/ads (the Graph API ads
// edge returns {"data":[{"id":...}], "paging":{...}}). It follows paging (via the opaque
// after cursor) so a large ad set is fully covered, bounded by adDiscoveryMaxPages. A returned
// ad with a missing/non-numeric id FAILS discovery (fail-closed) rather than being skipped —
// a skipped ad would make discovery look complete and let the cascade persist ACTIVE while
// that ad stays PAUSED.
func (c *Client) listAdIDs(ctx context.Context, adSetID string) ([]string, error) {
	var ids []string
	after := ""
	seen := make(map[string]struct{})
	for page := 0; page < adDiscoveryMaxPages; page++ {
		// Build the request path OURSELVES from the cursor. Do NOT reuse Meta's absolute
		// paging.next URL: it carries the access_token (and appsecret_proof) as query params,
		// which would then be sent in the URL and copied into apiError/transportError — and the
		// toggle service logs those errors. Passing only the opaque `after` cursor keeps the
		// credential out of any persisted/logged path.
		path := "/" + adSetID + "/ads?fields=id&limit=" + strconv.Itoa(adDiscoveryPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			// Data is a POINTER slice so an ABSENT/null `data` field is distinguishable from a
			// present-but-empty `{"data":[]}`. A malformed 2xx body like `{}` or `null` decodes
			// with Data == nil (field absent) and CANNOT prove the ad set has no ads, whereas an
			// intentional empty page is `{"data":[]}` (Data non-nil, len 0). Decoding both to a
			// plain nil slice would let a `{}` body read as "fully enumerated, zero ads" and flip
			// the campaign ACTIVE while ads stay PAUSED — the fail-open trap this cascade forbids.
			// Mirrors the LinkedIn discovery path's `Elements *[]...` presence check.
			Data *[]struct {
				ID string `json:"id"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		if resp.Data == nil {
			// 2xx but no `data` field: the body is malformed and we cannot prove the ad set's ads
			// were enumerated. Fail closed rather than report a spurious complete-empty result.
			return nil, fmt.Errorf("ad discovery for ad set %s returned a 2xx response with no data field; cannot confirm all ads were enumerated", adSetID)
		}
		for _, a := range *resp.Data {
			id := strings.TrimSpace(a.ID)
			if !numericIDRE.MatchString(id) {
				// The edge returned an ad but its id is missing/non-numeric — we can't PATCH
				// it. Silently skipping would make discovery look complete and let the cascade
				// persist ACTIVE while this ad stays PAUSED (the fail-open trap the
				// fail-not-truncate guards close). Fail instead.
				return nil, fmt.Errorf("ad discovery for ad set %s returned an ad with no usable id", adSetID)
			}
			ids = append(ids, id)
		}
		// No `next` link means this was the last page; an empty `after` cursor with a `next`
		// link present is malformed (can't advance) → treat as incomplete.
		if resp.Paging.Next == "" {
			return ids, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, fmt.Errorf("ad discovery for ad set %s has more pages but no cursor; cannot guarantee all ads were enumerated", adSetID)
		}
		if _, dup := seen[after]; dup {
			return nil, fmt.Errorf("ad discovery for ad set %s did not terminate (repeated paging cursor)", adSetID)
		}
		seen[after] = struct{}{}
	}
	// Reached the page cap with a cursor still pending: discovery is INCOMPLETE, so fail
	// rather than silently truncate.
	return nil, fmt.Errorf("ad discovery for ad set %s exceeded %d pages; too many ads to enumerate", adSetID, adDiscoveryMaxPages)
}

// partialCascadeError marks a status cascade that applied to SOME of the campaign/ad set/ads
// but then failed on another entity: the run state is PARTIALLY applied. Because the cascade
// order is status-dependent (on ACTIVATE the ad set/ads are flipped BEFORE the campaign; on
// PAUSE the campaign first), a partial error does NOT imply the campaign itself changed — only
// that the tree is not uniformly at the requested status. Its Unconfirmed() reports true so
// IsOutcomeUnconfirmed treats it as "may be applied — verify before retrying" rather than
// "not modified"; a retry re-runs the idempotent cascade. Mirrors the reddit client.
type partialCascadeError struct {
	stage string
	err   error
}

func (e *partialCascadeError) Error() string {
	// Does NOT assert which entity changed: with the status-dependent ordering a partial
	// cascade can occur before OR after the campaign flip. States only that the status change
	// is partially applied / unconfirmed.
	return "meta: status change partially applied (" + e.stage + " step failed; verify before retrying): " + e.err.Error()
}
func (e *partialCascadeError) Unwrap() error     { return e.err }
func (e *partialCascadeError) Unconfirmed() bool { return true }

// isMutatingMethod reports whether an HTTP method can create/modify server state,
// so a 3xx on it may hide a committed mutation. Mirrors the reddit client.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// doRequest performs a Graph API call and decodes the JSON body into out.
// It honors ctx via http.NewRequestWithContext. A 429 (rate-limited) response is
// retried up to retryMax times with a bounded backoff (honoring Retry-After when
// present), since CreateCampaign issues several sequential Graph API calls that
// can trip Meta's per-app/account rate limits mid-flow.
func (c *Client) doRequest(ctx context.Context, method, path string, body map[string]any, out any) error {
	return c.do(ctx, method, path, body, out, true)
}

// doCreate is doRequest for a POST that CREATES a node, where repeating the request
// is not safe.
//
// The retry loop cannot repeat a create on a throttle. This client's own premise —
// the one createOutcomeAmbiguous is built on — is that a throttle may arrive AFTER
// Meta committed the node: that is why an exhausted 429, and the far more common
// HTTP-400-with-rate-limit-code form, are classified as UNCONFIRMED rather than as
// clean rejections. Retrying inside doRequest would act on the opposite premise and
// re-POST a create that may already exist, producing two campaigns (or ad sets, or
// ads) with the same name inside ONE call. The find-by-name reconciliation that makes
// creates idempotent runs at the START of the flow, not between retry attempts, so it
// cannot see or clean up the duplicate — and a duplicate the caller never learns about
// is exactly the outcome this whole file exists to prevent.
//
// So a throttled create returns the rate-limit error immediately, and the caller
// classifies it through createOutcomeAmbiguous as UNCONFIRMED so the operator is told
// to verify the campaign in Ads Manager before retrying.
//
// It does NOT self-heal on the next run. findCampaignByName / findAdSetByName are
// gated on CampaignInput.ReconcileByName, which nothing in internal/dispatch sets —
// the reconciliation this file adds is dormant infrastructure awaiting a safe-resume
// signal through the orchestrator (see internal-platform-meta.md). Until that is
// wired, an UNCONFIRMED create is resolved by a person looking at Ads Manager, not by
// a later pass adopting the node. Losing the in-call retry still costs only an extra
// round trip on a throttle that was a clean pre-commit rejection; keeping it would
// risk a duplicate inside ONE call that no later pass — dormant or not — could
// distinguish from the original.
//
// Idempotent POSTs — the status updates in UpdateCampaignStatus and friends, which
// assert a desired state rather than creating a node — keep using doRequest and its
// retry, since repeating them changes nothing.
func (c *Client) doCreate(ctx context.Context, path string, body map[string]any, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out, false)
}

// do is the shared workhorse. retryThrottle=false suppresses ONLY the throttle retry;
// every other classification (transport ambiguity, oversized/unreadable bodies, the
// Retry-After abort) is identical, so a create and a read disagree about repeating a
// request and about nothing else.
func (c *Client) do(ctx context.Context, method, path string, body map[string]any, out any, retryThrottle bool) error {
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
			// A Do error that clearly happened BEFORE the request could be sent (DNS
			// failure, connection refused, no route) means NOT sent — return it plain
			// so callers treat a create as "not applied". A failure after a connection
			// was established (mid-flight timeout, EOF) is genuinely ambiguous: wrap it
			// as transportError so callers treat a create as "may exist". Mirrors the
			// reddit client.
			if isPreSendDialError(err) {
				return fmt.Errorf("meta API %s %s: %w", method, path, err)
			}
			return &transportError{Method: method, Path: path, Err: err}
		}

		// Read one byte past the cap so a truncation is detectable: io.LimitReader
		// returns EOF (not an error) at the limit, so an oversized body would
		// otherwise be silently truncated and mis-parsed as a valid short response.
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
		retryAfter := c.parseRetryAfter(resp)
		status := resp.StatusCode
		_ = resp.Body.Close()

		if readErr == nil && int64(len(raw)) > maxResponseBody {
			// Oversized body: we can't trust the payload, but the STATUS must still be
			// preserved for the same reason as a read failure below — a mutating 3xx/5xx
			// (or a 2xx) may reflect a committed create, and stripping the status would
			// mis-classify it as a definite failure and invite a duplicate on retry. A
			// 2xx is ambiguous (transportError); a non-2xx carries its status via
			// *APIError. (An oversized error/redirect body is anomalous, but we classify
			// on status, not payload.)
			if status >= 200 && status < 300 {
				// A 2xx with an oversized body is a SUCCESS when the caller decodes no
				// response (out == nil, e.g. a status update): there is nothing to parse, so
				// the unreadable body doesn't matter and the mutation is confirmed. Only when
				// we NEEDED the body (out != nil) is it ambiguous.
				if out == nil {
					return nil
				}
				return &transportError{Method: method, Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBody)}
			}
			// EnvelopeUnreadable, because this branch returns BEFORE env is unmarshalled
			// and could not have populated it anyway: raw holds only the first
			// maxResponseBody+1 bytes of a larger body, so it is truncated JSON. Without
			// the flag this is a bare 400, which createOutcomeAmbiguous reads as a clean
			// semantic rejection — and Meta's most common throttle shape is a 400 whose
			// rate-limit code sits in the body we just failed to read.
			return &APIError{
				StatusCode: status, Method: method, Path: path,
				Message:            fmt.Sprintf("response exceeds %d bytes", maxResponseBody),
				EnvelopeUnreadable: true,
			}
		}

		// Meta reports throttling either as HTTP 429 or, commonly, as HTTP 400 with
		// a Graph error envelope whose code is a known rate-limit code. Treat both
		// as retryable with the same bounded backoff. The envelope is only consumed
		// on the non-2xx paths (throttle detection here and the error/abort branches
		// below), so only unmarshal it then — a 2xx success body never populates it.
		var env graphErrorEnvelope
		if status < 200 || status >= 300 {
			_ = json.Unmarshal(raw, &env)
		}
		// isThrottle is the CLASSIFICATION — "Meta shed this request" — and is deliberately
		// kept separate from throttled, which is the narrower "and we are going to retry it".
		// Collapsing the two (clearing one flag for creates) also discarded the classification,
		// and the terminal read-error path below needs it: it is what tells
		// createOutcomeAmbiguous that a create may have committed before the shed.
		isThrottle := status == http.StatusTooManyRequests ||
			(status < 200 || status >= 300) && env.Error != nil && graphRateLimitCodes[env.Error.Code]
		// A create never repeats itself (see doCreate), so it never enters the retry branch —
		// but it is still a throttle, and must be consumed as final while carrying everything
		// that says so.
		throttled := isThrottle && retryThrottle

		// A read error (e.g. connection closed early on a mismatched Content-Length)
		// must not be treated as a complete response: even if the partial body
		// happens to parse, propagate the error rather than reporting a false
		// success. But do NOT short-circuit a throttled response we're about to
		// retry (its body is discarded anyway) — only fail when we would otherwise
		// consume this response as final.
		if readErr != nil && (!throttled || attempt >= retryMax) {
			// A read failure on a 2xx is AMBIGUOUS: Meta committed the mutation but we
			// couldn't read the result — wrap it as transportError so a create is
			// treated as "may exist". Mirrors the reddit client wrapping 2xx read/decode
			// failures as transportError.
			if status >= 200 && status < 300 {
				// As with the oversized case: a 2xx is a SUCCESS when out == nil (no response
				// to read), so an unreadable body doesn't downgrade a confirmed mutation to
				// ambiguous. Only a caller that needed the body sees a transportError.
				if out == nil {
					return nil
				}
				return &transportError{Method: method, Path: path, Err: fmt.Errorf("read response body: %w", readErr)}
			}
			// A read failure on a NON-2xx still must preserve the HTTP status: a
			// mutating 3xx (redirect, not followed) or 5xx may have committed the create
			// before the unreadable body, and createOutcomeAmbiguous classifies on the
			// *APIError status. Returning a plain error here would strip the status and
			// silently turn an ambiguous create into a definite "failed" — the exact
			// duplicate-on-retry risk the no-follow + ambiguity handling exists to close.
			//
			// A truncated read does NOT imply an unusable envelope: the common shape is a
			// complete JSON body followed by a connection closed early on a mismatched
			// Content-Length, so `raw` often parses. Whatever did parse is carried onto the
			// APIError — and for a shed create that is load-bearing, not decoration. Meta
			// reports rate limiting as an HTTP 400 with a Graph rate-limit code far more often
			// than as a 429, and createOutcomeAmbiguous classifies on Code for exactly that
			// reason. Dropping the code here would hand it a bare 400, which reads as a clean
			// semantic rejection — and a blind retry on a create that Meta may already have
			// committed duplicates a PAID campaign.
			readErrAPI := &APIError{
				StatusCode: status, Method: method, Path: path,
				Message: fmt.Sprintf("read response body: %v", readErr),
			}
			if env.Error != nil {
				readErrAPI.Type = env.Error.Type
				readErrAPI.Code = env.Error.Code
				readErrAPI.FBTraceID = env.Error.FBTraceID
			} else {
				// The truncated body did NOT parse, so there is no code to carry and the
				// paragraph above does not apply. A missing code here means "we never read
				// it", not "Meta sent none" — the same unknowable state as the oversized
				// branch, and it must classify the same way rather than defaulting to a
				// bare status that reads as a clean rejection.
				readErrAPI.EnvelopeUnreadable = true
			}
			return readErrAPI
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
					// Report the RAW Retry-After header as authoritative: parseRetryAfter
					// CLAMPS an oversized reset to maxRetryWait+1s (a sentinel used only to
					// trip this cap comparison), so `retryAfter` here can read "1m1s" even
					// when the server sent "600" or a far-future HTTP-date. The raw header
					// is what actually needs to be debugged against upstream.
					rawRetryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
					abortErr := &APIError{
						StatusCode: status, Method: method, Path: path,
						Message: fmt.Sprintf("rate-limit reset (Retry-After: %q) exceeds max wait %s; aborting", rawRetryAfter, maxRetryWait),
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
				// Non-Graph or malformed error body: surface a truncated snippet of the
				// raw body so the real reason isn't lost. REDACT FIRST. This branch is
				// reached exactly when the body is NOT a Graph diagnostic — a proxy/CDN/WAF
				// page, an HTML error, or a reflection of the request we just sent. This
				// client authenticates with an `Authorization: Bearer` HEADER (doRequest
				// sets it and never appends access_token to the query), so a reflection
				// that echoes request headers echoes a live token — which is why
				// redactCredentials handles the Bearer form as well as key=value. The
				// query-string form is not this client's own auth, but it still appears in
				// bodies that echo a Meta-constructed paging.next URL, so both are covered.
				// safeErrSummary at the log call bounds and sanitizes this text but does NOT
				// redact it, so the only place the credential can be removed is here, before
				// it enters the error chain at all.
				apiErr.Message = truncate(c.redactSecrets(snippet), 300)
			}
			return apiErr
		}

		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				// A 2xx we can't decode is AMBIGUOUS: Meta committed the mutation but we
				// can't read the id. Wrap as transportError so a create is treated as
				// "may exist". Mirrors the reddit client.
				return &transportError{Method: method, Path: path, Err: fmt.Errorf("decode response: %w", err)}
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
	raw = strings.TrimSpace(raw)
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

// validateVariantImageURL validates an OPTIONAL per-variant creative image URL.
// An empty value is valid and means "no image" — the creative is then built as a
// bare link ad, exactly as before this field existed.
//
// This runs BEFORE any mutating call (alongside the copy-limit checks) on purpose.
// The image is attached per-variant as link_data.picture inside the creative loop,
// where a rejection is non-fatal — by then the paid campaign and ad set already
// exist. A malformed URL is a deterministic caller error that we can detect with no
// network at all, so detecting it up front turns "orphaned paid campaign with no
// ads" into a clean pre-spend rejection. Mirrors validateRegistrationURL's checks
// and its rationale.
func validateVariantImageURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	// Require an absolute URL with a real hostname, for the same reason as
	// validateRegistrationURL: parsed.Host can be a port-only authority (e.g.
	// "https://:443" parses to Host==":443" with an empty Hostname()).
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return fmt.Errorf("image URL is not a valid URL")
	}
	// Reject embedded userinfo (user[:password]@host). Meta FETCHES this URL
	// server-side, so credentials embedded in it would be handed to Meta in the
	// creative body as link_data.picture — a basic-auth secret must not travel that
	// path. (The URL is deliberately kept out of error strings and Steps; this check
	// closes the remaining route by which it reaches a third party at all.) Mirrors
	// validateRegistrationURL.
	if parsed.User != nil {
		return fmt.Errorf("image URL must not contain embedded credentials (userinfo)")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("image URL must use HTTPS")
	}
	return nil
}

// validateVariantImage validates a variant's OPTIONAL creative image across BOTH
// ways of supplying one, and is the single pre-spend gate for the image fields.
//
// Meta offers two mutually-exclusive ways to put an image on a link creative, and
// this client supports both:
//
//   - BY URL — ImageURL lands in object_story_spec.link_data.picture. Meta fetches
//     the URL server-side, so this service never dereferences it and brings none of
//     the SSRF surface that would come with fetching. One JSON create, no upload.
//   - BY STORED BYTES — ImageAssetID names an asset uploaded against the brief; the
//     dispatcher resolves it to ImageBytes, which are POSTed to /act_<id>/adimages
//     and the returned account-scoped hash lands in link_data.image_hash.
//
// They are exclusive because the FIELDS are: Meta documents picture as "Specify
// this field or image_hash but not both." A variant supplying both is therefore a
// caller error with no correct interpretation — picking one silently would attach
// an image the caller did not choose — so it is REFUSED HERE, locally, before any
// credential is used or any upstream call is made. Deferring to Meta would mean
// discovering it at the per-variant creative step, where the paid campaign and ad
// set already exist and the rejection is non-fatal: money spent, no ad.
//
// Both empty is valid and means "no image": the creative is built as a bare link
// ad, exactly as before either field existed.
func validateVariantImage(v AdVariant) error {
	hasURL := strings.TrimSpace(v.ImageURL) != ""
	// The ASSET REFERENCE is what the caller supplied, so it is what the exclusivity
	// check must read. Keying off ImageBytes instead would make the refusal depend on
	// whether resolution had already run, so a config naming both would slip through
	// on any path that validates before resolving.
	hasAsset := strings.TrimSpace(v.ImageAssetID) != "" || len(v.ImageBytes) > 0
	if hasURL && hasAsset {
		return fmt.Errorf("image URL and image asset id are mutually exclusive; Meta accepts link_data.picture or link_data.image_hash but not both — supply one")
	}
	return validateVariantImageURL(v.ImageURL)
}

// validateGeoTargets uppercases, trims, and filters to ISO-2 codes; defaults to
// ["US"] when nothing valid remains (mirrors validateGeoTargets).
func validateGeoTargets(geoTargets []string) []string {
	valid := make([]string, 0, len(geoTargets))
	seen := make(map[string]struct{}, len(geoTargets))
	for _, g := range geoTargets {
		up := strings.ToUpper(strings.TrimSpace(g))
		// Check shape and ISO 3166-1 alpha-2 membership (so a well-shaped but bogus
		// code like "XX"/"ZZ" is dropped), and exclude countries Meta does not allow
		// as ad targets (see metaIneligibleCountries) — ISO membership is not the
		// same as Meta targeting eligibility.
		if _, ok := iso3166Alpha2[up]; !ok || !geoCodeRE.MatchString(up) || metaIneligibleCountries[up] {
			continue
		}
		// Dedupe in first-seen order so ["us","US"] yields ["US"], not ["US","US"].
		if _, dup := seen[up]; dup {
			continue
		}
		seen[up] = struct{}{}
		valid = append(valid, up)
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

// iso3166Alpha2 (the large ISO 3166-1 alpha-2 lookup table) lives in
// countries.go to keep the static data out of the core client logic.

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
		addPlatform("audience_network")
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
	objective := objectiveLabel(defaultObjective(strings.ToLower(strings.TrimSpace(in.Objective))))
	project := strings.ReplaceAll(strings.TrimSpace(in.Project), "|", "-")
	return fmt.Sprintf("Events | %s | %s | %s | Intent | Social | %s | MoFU", event, region, objective, project)
}

// metaUTMParams returns the exact set of utm_* parameters this client generates
// for a click URL. It is the single source of truth for both the real click URL
// (buildUTMURL) and the sanitized display URL (displayMetaUTMURL), so the display
// allowlist can never drift from what is actually sent to Meta. Mirrors the
// reddit client's redditUTMParams.
func metaUTMParams(in CampaignInput, variantIndex int) map[string]string {
	eventName := strings.TrimSpace(in.EventName)

	slug := in.EventSlug
	if slug == "" {
		slug = collapseSpacesToDash(strings.ToLower(eventName))
	}

	campaign := in.HSToken
	if campaign == "" {
		campaign = slug
	}

	return map[string]string{
		"utm_source":   "meta",
		"utm_medium":   "paid-social",
		"utm_campaign": campaign,
		"utm_term":     strings.ToLower(collapseSpacesToDash(eventName)),
		"utm_content":  fmt.Sprintf("variant-%d", variantIndex+1),
	}
}

// buildUTMURL mirrors buildMetaUtmUrl. It returns the REAL click URL sent to Meta
// (link_data.link): the caller's original query and fragment are preserved and
// the generated utm_* params are merged in. This is intentionally NOT sanitized —
// the ad must land on the caller's full destination. For a value safe to persist
// in Steps, use displayMetaUTMURL.
func buildUTMURL(in CampaignInput, variantIndex int) string {
	// Trim defensively rather than trusting callers to pre-normalize: CreateCampaign
	// trims RegistrationURL/EventName in place today, but this helper is also called
	// directly from tests, and untrimmed inputs would otherwise reintroduce a
	// leading/trailing dash in utm_term or a parse failure from a padded URL.
	base := strings.TrimSpace(in.RegistrationURL)

	utm := metaUTMParams(in, variantIndex)

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

// displayMetaUTMURL builds a click URL safe to persist in Steps / return to
// callers: it strips any userinfo and any PRE-EXISTING query parameters from the
// registration URL (which may carry secrets like ?token=...) and any fragment,
// keeping ONLY the generated utm_* parameters. The full URL — including the
// caller's original query — is still sent to Meta as the real link (buildUTMURL);
// only this display copy is sanitized. variantIndex mirrors buildUTMURL. Mirrors
// the reddit client's displayRedditUTMURL.
func displayMetaUTMURL(in CampaignInput, variantIndex int) string {
	u, err := url.Parse(strings.TrimSpace(in.RegistrationURL))
	if err != nil {
		// Fall back to a plain redaction (scheme+host+path) if the URL won't parse —
		// never return the raw value with its secrets.
		return redactURL(in.RegistrationURL)
	}
	u.User = nil    // drop any basic-auth userinfo
	u.Fragment = "" // a fragment can carry sensitive data; drop it for display
	// Rebuild the query from ONLY the utm_* params THIS client generates (with our
	// values), discarding the caller's entire original query. Filtering the merged
	// query by a "utm_" prefix would be unsafe: a caller-supplied ?utm_secret=... or
	// ?utm_source=<override> would survive. An explicit allowlist from the shared
	// generator (metaUTMParams) is the source of truth.
	safe := url.Values{}
	for k, v := range metaUTMParams(in, variantIndex) {
		safe.Set(k, v)
	}
	u.RawQuery = safe.Encode()
	return u.String()
}

// redactURL returns a URL safe to persist in a result step: scheme://host/path
// only, dropping the query and fragment (which can carry sensitive tokens) and
// any userinfo. If the input does not parse as an absolute URL, only the portion
// before any '?' or '#' is kept, and a value that still contains userinfo ("@")
// is dropped entirely rather than risk echoing a credential. Mirrors the reddit
// client's redactURL.
func redactURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if u, err := url.Parse(trimmed); err == nil && u.IsAbs() && u.Host != "" {
		redacted := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		return redacted.String()
	}
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if strings.Contains(trimmed, "@") {
		return "[unparseable-url-redacted]"
	}
	return trimmed
}

var wsRE = regexp.MustCompile(`\s+`)

// collapseSpacesToDash replaces runs of whitespace with a single dash, matching
// the TS `.replace(/\s+/g, '-')`.
func collapseSpacesToDash(s string) string {
	return wsRE.ReplaceAllString(s, "-")
}

// credentialRE matches credential-shaped material that a reflected or proxied
// error body can carry back: this client authenticates with access_token and
// appsecret_proof as QUERY PARAMETERS, so any upstream that echoes the request
// line echoes a live token. The three forms below are the ones actually
// observed in Meta-adjacent error bodies — query/form pairs, JSON members, and
// an Authorization header rendered in a proxy debug page.
// bearerScheme is the Authorization scheme prefix, matched case-insensitively to
// decide WHICH credentialRE alternative fired before any delimiter search.
const bearerScheme = "bearer"

var credentialRE = regexp.MustCompile(
	`(?i)("?(?:access_token|appsecret_proof|client_secret|refresh_token)"?\s*[=:]\s*"?[^&\s"'<>,}]+"?)|(bearer\s+[A-Za-z0-9._~+/=-]+)`)

// minRedactableSecretLen is the shortest configured secret that redactSecrets will
// substring-replace. A one- or two-character token would match all over an ordinary
// error body and turn the snippet into confetti, destroying the diagnostic without
// protecting anything worth protecting.
const minRedactableSecretLen = 8

// redactSecrets scrubs an untrusted upstream body for this client's own credential
// FIRST, by exact value, and only then applies the shape-based pass.
//
// The order matters, and the shape-based pass alone is not sufficient. credentialRE's
// Bearer alternative matches a restricted token alphabet ([A-Za-z0-9._~+/=-]), but
// Credentials.AccessToken is accepted after trimming and nothing else — it is sent
// verbatim. Meta's own app access tokens are of the form "{app-id}|{app-secret}", and
// '|' is outside that alphabet: shape-based redaction of "Bearer 12345|SECRET" stops
// at the pipe and yields "Bearer [REDACTED]|SECRET", which carries the app secret into
// APIError.Message while LOOKING handled. Replacing the configured token by exact
// value cannot be defeated by an unanticipated character, because it does not guess at
// the token's shape at all.
//
// The shape-based pass still runs afterwards, because a reflected body can also carry
// credentials this client never held — a Meta-constructed paging.next URL with its own
// access_token, or another tenant's token echoed by a shared proxy.
func (c *Client) redactSecrets(s string) string {
	if tok := c.creds.AccessToken; len(tok) >= minRedactableSecretLen {
		s = strings.ReplaceAll(s, tok, "[REDACTED]")
	}
	return redactCredentials(s)
}

// redactCredentials removes credential values from an untrusted upstream body
// before it is stored in an error. The KEY is kept — knowing that the body
// echoed an access_token is itself diagnostic — and only the VALUE is replaced,
// so the snippet stays useful for debugging without carrying a live secret into
// the error chain, the logs, or a client-facing 5xx.
func redactCredentials(s string) string {
	return credentialRE.ReplaceAllStringFunc(s, func(m string) string {
		// Which alternative matched has to be decided BEFORE looking for a
		// delimiter, not inferred from one. Base64 padding puts '=' INSIDE a bearer
		// token, so a delimiter-first search on "Bearer abc==" splits at the padding
		// and returns "Bearer abc=[REDACTED]" — nearly the whole credential, with the
		// redaction marker present to make it look handled.
		if len(m) >= len(bearerScheme) && strings.EqualFold(m[:len(bearerScheme)], bearerScheme) {
			// "Bearer <token>": keep the scheme, drop everything after it.
			if i := strings.IndexAny(m, " \t"); i >= 0 {
				return m[:i+1] + "[REDACTED]"
			}
			return "[REDACTED]"
		}
		// key=value / "key": "value" — keep the key, drop the value. The key is the
		// diagnostic; a value never is.
		if i := strings.IndexAny(m, "=:"); i >= 0 {
			return m[:i+1] + "[REDACTED]"
		}
		return "[REDACTED]"
	})
}

// scrubURLFromErr renders a variant-failure error for a PERSISTED step, removing
// the caller's image URL before the message reaches the sink.
//
// Steps are persisted and logged, and the image URL is caller-supplied data that
// may be a pre-signed URL — a bearer credential whose signature grants time-boxed
// read access. It is now sent to Meta as link_data.picture, and Meta echoes a
// rejected parameter's value in error.message, which `do` copies verbatim into
// APIError.Message. Every hand-built error string in this file already omits the
// URL; this closes the one path that does not, by scrubbing at the sink rather
// than at each error site, so a message reflected from upstream cannot smuggle it
// through. Mirrors displayMetaUTMURL's reason for existing: the full URL still
// goes to Meta, only the persisted copy is sanitized.
//
// The rule is STRUCTURAL, not a search for the secret in the text: when the image
// URL carries a query or fragment — the part that holds a pre-signed signature —
// upstream-derived text is NEVER emitted. The step becomes the URL's redactURL
// form (scheme+host+path, so it still says WHICH image failed) plus a fixed note.
//
// An earlier revision replaced the URL by exact substring match and then verified
// the result carried no recognizable fragment of the secret. That is not sound.
// The text arriving here has been through transformations that the replacement
// cannot invert and the verifier cannot enumerate: `do` truncates a non-Graph body
// at 300 runes (clipping the signature mid-value), and a proxy/WAF may re-encode,
// line-wrap, or otherwise re-render it. A substring verifier only rejects the
// residues it thought to look for — an echo of "?sig=SECRET_SIG" wrapped to
// "?sig=SEC\nRET_SIG" defeats both the replacement AND a prefix scan, because no
// contiguous run of the value at or above any sensible minimum length survives.
// Proving arbitrary transformed text clean is not something substring checks can
// do, so this no longer tries: it withholds by construction on the only input
// class where a secret can exist.
//
// When the URL has NO query or fragment there is no secret to protect, so the
// message is emitted with the URL replaced by its redactURL form — the diagnostic
// is kept wherever keeping it is safe. An empty imageURL leaves it untouched.
//
// Every return path is clamped to max, including the withheld one: redactURL keeps
// the caller-controlled path, so an over-long path must not produce an unbounded
// persisted Step.
func scrubURLFromErr(err error, imageURL string, max int) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	raw := strings.TrimSpace(imageURL)
	if raw == "" {
		return truncate(msg, max)
	}
	if urlHasSecretMaterial(raw) {
		// Fail closed: withhold upstream text entirely rather than emit text that
		// no substring check can prove free of the signature.
		return truncate(fmt.Sprintf("%s (message withheld: the image URL carries credentials that upstream text may echo)", redactURL(raw)), max)
	}
	// No query/fragment: nothing secret to leak. Replace the URL (and the form an
	// upstream may have percent-encoded) with its redactURL form and keep the message.
	msg = strings.ReplaceAll(msg, raw, redactURL(raw))
	if esc := url.QueryEscape(raw); esc != raw {
		msg = strings.ReplaceAll(msg, esc, redactURL(raw))
	}
	return truncate(msg, max)
}

// urlHasSecretMaterial reports whether raw carries a query or fragment — the
// components in which a pre-signed URL carries its signature. Scheme, host and
// path are not secret material: redactURL deliberately preserves them so a step
// still identifies which image failed.
//
// A value that does not parse is treated as SECRET-BEARING. An unparseable string
// is exactly the case where the delimiter scan is least trustworthy, and the safe
// answer under a fail-closed policy is to withhold.
func urlHasSecretMaterial(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	if u, err := url.Parse(trimmed); err == nil {
		return u.RawQuery != "" || u.Fragment != "" || u.ForceQuery
	}
	return true
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
	// ImageURL is an OPTIONAL https URL to a single image for this variant. When
	// set, CreateCampaign attaches it to the creative as
	// object_story_spec.link_data.picture — the documented by-URL field, which Meta
	// fetches server-side and saves into the ad account's image library — so the ad
	// renders as a single-image ad. When empty the creative is built exactly as
	// before — a bare link ad — so this field is additive and no existing caller
	// changes behavior.
	//
	// The URL is fetched by META, not by this service: the client never dereferences
	// it, which is why no SSRF egress control is needed here. It is still validated
	// up front (absolute https, no userinfo) because an unusable URL must fail
	// BEFORE the campaign is created, not at the per-variant creative step where
	// the paid campaign already exists.
	//
	// ImageURL and ImageAssetID/ImageBytes are the two ways to put an image on a
	// creative and are MUTUALLY EXCLUSIVE on a single variant, because the fields
	// they map to are: Meta documents link_data.picture as "Specify this field or
	// image_hash but not both." Supplying both is refused up front by
	// validateVariantImage — locally, before any upstream call — rather than left
	// for Meta to reject after the campaign and ad set already exist.
	ImageURL string

	// ImageAssetID references an image previously uploaded against this brief
	// (UploadCreativeAsset). It is a CONFIG field: the caller sets it (JSON key
	// imageAssetId, matched case-insensitively like every other AdVariant field,
	// since AdVariant carries no json tags), and the dispatcher resolves it to
	// ImageBytes before the client is constructed. The client itself never reads it
	// — it works only from the resolved bytes — so it exists here to carry the
	// caller's reference through decode and to let validation see that a variant
	// asked for an image by id.
	//
	// Empty and ImageURL empty → a link-only creative (the pre-image behaviour).
	ImageAssetID string

	// ImageBytes is the resolved image, populated ONLY by the dispatcher from
	// ImageAssetID and never from caller JSON (json:"-", so a config body cannot
	// inject raw bytes and a marshalled variant never carries a multi-megabyte
	// blob). When set, CreateCampaign uploads it to the ad account and attaches the
	// resulting account-scoped hash as object_story_spec.link_data.image_hash — the
	// by-STORED-BYTES path, as against ImageURL's by-URL path.
	//
	// Depending on bytes+MIME rather than on the asset store is what will let a
	// future video/carousel variant carry its own resolved bytes through the same
	// field without the client learning a new dependency.
	ImageBytes []byte `json:"-"`

	// ImageMIME is the VERIFIED content type of ImageBytes (image/png or
	// image/jpeg), sniffed at upload rather than trusted from the client. It labels
	// the multipart part on the /adimages call. Like ImageBytes it is set by the
	// dispatcher, not by caller JSON.
	ImageMIME string `json:"-"`
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
	// InstagramUserID is the Instagram account (IGSID) bound to the ad creative. It
	// is REQUIRED for an ad set that requests any Instagram placement (the default
	// placements include Instagram Feed): without it Meta flags the ad "Please add
	// Instagram account" and refuses to publish, even though the Page's connected
	// Instagram account shows pre-selected in the editor. Sent only when supplied, so
	// Facebook-only flows are unchanged. (Legacy Graph field name: instagram_actor_id.)
	InstagramUserID string
	// DSABeneficiary and DSAPayor are the EU Digital Services Act "advertiser" and
	// "payer" disclosures set on the ad set. Meta holds the ad set unpublishable
	// ("Please add Advertiser" / "Please add Payer") for regulated locations until both
	// are present. Both are sent only when supplied, so flows that target no regulated
	// location are unchanged.
	DSABeneficiary string
	DSAPayor       string
	HSToken        string
	Variants       []AdVariant
	// ReconcileByName opts THIS call in to looking the campaign (and its ad set) up by
	// name before creating, and reusing a PAUSED match instead of creating a second one.
	//
	// It is opt-in, and deliberately false everywhere today, because the name is NOT a
	// brief-unique key: buildCampaignName is event/region/objective/project only, so an
	// unconditional lookup reuses any campaign that happens to share those four segments
	// — including one belonging to a DIFFERENT brief, which would attach two briefs to
	// one upstream campaign and make their spend indistinguishable.
	//
	// The reuse is also wrong on the one path that reaches a fresh create with a
	// same-named campaign already upstream: DELETE frees the (brief, platform) slot
	// LOCALLY and never touches the ad platform (docs/api-catalog.md), so the documented
	// delete → re-dispatch flow — the supported way to fix a campaign created with the
	// wrong budget — would find the old campaign by name (the budget is not a name
	// segment), reuse it and its ad set, and silently re-run the wrong budget while
	// reporting success.
	//
	// The retry case this lookup exists to protect (an ambiguous create that may have
	// left a campaign upstream) does NOT reach it today: the orchestrator retains the
	// partial row and answers "reconciliation required" rather than re-dispatching
	// (internal/service/orchestrator.go, the retained-partial-orphan branch). When that
	// reconcile path lands (LFXV2-2665) it is the caller that knows it is resuming a
	// specific dispatch generation, so it sets this flag; the client cannot infer it.
	ReconcileByName bool
}

// CampaignResult mirrors MetaCampaignCreateResult.
type CampaignResult struct {
	Platform     string
	CampaignName string
	CampaignID   string
	// AccountID is the ad account the campaign was CREATED under, stored verbatim as the
	// connection carries it — this field applies no normalisation of its own. It is Meta's
	// documented "act_<digits>" form because design/connection.go constrains the stored
	// connection id to ^act_[0-9]+$, not because anything here enforces it; the dispatcher's
	// normalizeMetaAccountID re-derives that shape on both sides of the comparison rather than
	// trusting this field to have it. metaCreationAccountID reads the value back so a later
	// read/toggle resolving to a DIFFERENT account is refused rather than addressing the
	// stored campaign id under the wrong account. This struct is marshalled UNTAGGED, so
	// the persisted key is the Go field name "AccountID" — the reader matches that.
	AccountID string
	AdSetName string
	AdSetID   string
	AdCount   int
	MetaURL   string
	Steps     []string
	// Ads carries one entry per SUCCESSFULLY-created ad (so len(Ads) == AdCount), in
	// variant order, each with its ad/creative ids and the image_hash attached to its
	// creative. A per-variant failure contributes a Steps line, not an Ads entry.
	Ads []AdResult
}

// AdResult is one successfully-created ad's identifiers, recorded per variant.
//
// ImageHash is the account-scoped hash of the image attached to this ad's creative,
// and is set ONLY for the by-stored-bytes path — it is empty both for a link-only
// creative and for a by-URL (link_data.picture) creative, which carries no hash by
// construction. It is what lets a reconcile pass tell which uploaded asset backs a
// live ad without re-deriving it, and it round-trips through the persisted Result
// blob (metaAdSetID reads that blob back as a CampaignResult).
//
// No json tags, matching CampaignResult's fields, so the marshalled keys stay the Go
// field names on both the write (campaignFromMeta) and read sides.
type AdResult struct {
	Variant    int // 1-based, matching the "Variant N" creative name and utm_content=variant-N
	AdID       string
	CreativeID string
	ImageHash  string
}

// ---------------------------------------------------------------------------
// CreateCampaign (mirrors executeMetaCampaignCreation)
// ---------------------------------------------------------------------------

// CreateCampaign creates a PAUSED Meta campaign, ad set, and one ad per valid
// variant. It faithfully ports executeMetaCampaignCreation: per-variant ad
// failures are recorded in Steps rather than aborting the whole operation.
//
// PARTIAL-RESULT CONTRACT: on a downstream failure AFTER the campaign and/or ad
// set already exist, CreateCampaign returns a NON-NIL *CampaignResult (carrying
// the created CampaignID / AdSetID / CampaignName, plus the steps so far) TOGETHER
// WITH a non-nil error. This is deliberate so the orphaned paid resource is
// identifiable for cleanup/reconciliation. It also applies to an AMBIGUOUS
// campaign-create failure (a timeout or 5xx that may have committed the create
// before erroring): the result then carries the deterministic CampaignName even
// though no id was read. Callers MUST NOT follow the usual
// `if err != nil { return err }` pattern that discards the result: inspect the
// returned *CampaignResult (CampaignID / CampaignName) even when err != nil to
// reconcile or avoid duplicate creation, since Meta exposes no create idempotency
// key. Before the campaign POST plausibly reached Meta (a clear pre-create or
// validation failure), a failure returns (nil, err) as usual.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	steps := []string{}

	if len(in.Variants) == 0 {
		return nil, fmt.Errorf("at least one ad variant is required for Meta campaign creation")
	}

	// Reject any variant missing primary text or headline by NAMING its index,
	// rather than silently dropping it. Silent filtering would renumber the
	// surviving variants, so the ad numbering, creative name ("Variant N"), and
	// utm_content=variant-N would no longer line up with the caller's original
	// input ordering — a surprising mismatch. A partially-specified variant is a
	// caller error, so fail fast (consistent with every other up-front check here).
	for i, v := range in.Variants {
		if strings.TrimSpace(v.PrimaryText) == "" || strings.TrimSpace(v.Headline) == "" {
			return nil, fmt.Errorf("variant %d must have non-empty primary text and headline", i+1)
		}
	}
	validVariants := in.Variants

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
		creativeName := fmt.Sprintf("%s - Variant %d", strings.TrimSpace(in.EventName), i+1)
		if n := utf8.RuneCountInString(creativeName); n > maxCreativeNameChars {
			return nil, fmt.Errorf("variant %d ad-creative name is %d characters; Meta allows at most %d (shorten the event name)", i+1, n, maxCreativeNameChars)
		}
		// The OPTIONAL creative image is validated here, with the rest of the
		// deterministic per-variant checks, so a malformed URL — or a variant naming
		// BOTH an image URL and an image asset — fails before any paid resource
		// exists rather than at the non-fatal per-variant creative create.
		if err := validateVariantImage(v); err != nil {
			return nil, fmt.Errorf("variant %d: %w", i+1, err)
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
	// time.Parse with this layout rejects BOTH a malformed string and a
	// well-formed-but-impossible date (e.g. 2026-13-40), so the error is about an
	// invalid calendar VALUE, not merely a bad format.
	startDate, err := time.Parse("2006-01-02", in.StartDate)
	if err != nil {
		return nil, fmt.Errorf("invalid start date %q — expected a real calendar date in YYYY-MM-DD format", in.StartDate)
	}
	endDate, err := time.Parse("2006-01-02", in.EndDate)
	if err != nil {
		return nil, fmt.Errorf("invalid end date %q — expected a real calendar date in YYYY-MM-DD format", in.EndDate)
	}
	// Compare the parsed time.Time values rather than the raw strings: both are
	// already parsed here, so !endDate.After(startDate) states the intent directly
	// instead of relying on lexicographic ordering of the date strings.
	if !endDate.After(startDate) {
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

	// Normalize RegistrationURL in place so validation and UTM construction see the
	// same value: validateRegistrationURL trims before parsing, but buildUTMURL reads
	// in.RegistrationURL directly — a padded URL like " https://x/ " would otherwise
	// pass validation yet be concatenated un-trimmed into the creative click URL,
	// producing a malformed parse. Trim once here, ahead of both consumers.
	in.RegistrationURL = strings.TrimSpace(in.RegistrationURL)

	// Trim the disclosure/identity fields once here so the "sent only when supplied"
	// guards below treat a whitespace-only value as absent (not as a blank string that
	// Meta would reject) and every consumer sees the same normalized value.
	in.InstagramUserID = strings.TrimSpace(in.InstagramUserID)
	in.DSABeneficiary = strings.TrimSpace(in.DSABeneficiary)
	in.DSAPayor = strings.TrimSpace(in.DSAPayor)

	// A SUPPLIED InstagramUserID must be well-formed. It is optional (an empty value
	// is a legitimate Facebook-only campaign and stays allowed), but when present it
	// is only consumed at the CREATIVE POST — which runs after the campaign and ad set
	// already exist, and where a 4xx is treated as a non-fatal per-variant failure.
	// A malformed IGSID would therefore surface as a created_degraded campaign with no
	// publishable ad: a billable resource that can never serve. The value is knowable-
	// bad here, so reject it before the first mutating call, the same gate PageID and
	// PixelID already get (Meta object ids are decimal strings).
	if in.InstagramUserID != "" && !numericIDRE.MatchString(in.InstagramUserID) {
		return nil, fmt.Errorf("instagramUserId %q is malformed: Meta Instagram account IDs (IGSID) are numeric strings", in.InstagramUserID)
	}

	// The two EU DSA disclosures are attached to the ad set independently, so exactly
	// one could otherwise be sent. Meta requires BOTH to publish an ad set targeting a
	// regulated location (docs/api-catalog.md), so a one-sided pair is deterministically
	// incomplete: it either gets the ad set rejected after the campaign exists, or leaves
	// it unpublishable. Unlike Meta's other publish-time requirements, one-sidedness is
	// knowable HERE, so reject it before any mutating call. Both absent remains valid —
	// that is the ordinary non-regulated flow and must not break.
	if (in.DSABeneficiary == "") != (in.DSAPayor == "") {
		supplied, missing := "dsaBeneficiary", "dsaPayor"
		if in.DSABeneficiary == "" {
			supplied, missing = "dsaPayor", "dsaBeneficiary"
		}
		return nil, fmt.Errorf("EU DSA disclosures are incomplete: %s was supplied without %s; Meta requires both to publish a regulated ad set — supply both or omit both", supplied, missing)
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
	// "Supplied geos" means NON-BLANK entries: a caller passing only whitespace
	// (e.g. []string{"   "}) is semantically the same as passing none, which
	// legitimately defaults to US — so the dropped/fallback checks below must not
	// treat it as an explicit request and error with an empty "(dropped: )" list.
	suppliedGeos := 0
	for _, g := range in.GeoTargets {
		if strings.TrimSpace(g) != "" {
			suppliedGeos++
		}
	}
	// Surface geos that were supplied but dropped by validateGeoTargets (bogus/
	// non-ISO codes, or Meta-ineligible/sanctioned countries like IR/CU/KP/RU) as
	// an explicit step, so a caller who mixed eligible + ineligible codes isn't
	// left believing an excluded country is being targeted. This mirrors the
	// regulated-country (SG/TW/KR) step emitted below. Skip the note when the only
	// difference is the empty-input US fallback.
	var droppedGeos []string
	if suppliedGeos > 0 {
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
	if suppliedGeos > 0 && len(allGeo) == 1 && allGeo[0] == "US" {
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

	// Step 2a: reconcile by name BEFORE creating. buildCampaignName is fully
	// deterministic (same brief → same name), so a retry after a prior
	// UNCONFIRMED/malformed-success outcome reaches the exact same name — but
	// Meta enforces no name uniqueness and would silently create a SECOND paid
	// campaign rather than reject the duplicate (unlike Google/Microsoft, which
	// error on a duplicate name and let the client self-heal by re-resolving the
	// id). Looking the name up first, instead of reacting to a duplicate-name
	// error that Meta never sends, is the only way to close that window.
	//
	// GATED on in.ReconcileByName, and that flag is false everywhere today. The name is
	// not a brief-unique key and the orchestrator does not yet re-dispatch a retained
	// partial, so an UNCONDITIONAL lookup cannot help the retry case it was written for
	// and does harm the two cases it can actually reach — a different brief sharing the
	// four name segments, and the documented delete → re-dispatch flow, which exists to
	// correct a campaign's config and would instead silently reuse the old one. See the
	// field's doc on CampaignInput for the full reasoning and for who sets it
	// (LFXV2-2665's reconcile path, which knows it is resuming one dispatch generation).
	var existingCampaignID string
	var lookupErr error
	if in.ReconcileByName {
		existingCampaignID, lookupErr = c.findCampaignByName(ctx, accountID, campaignName, objParams.CampaignObjective)
	}
	if lookupErr != nil {
		// A DEFINITE conflict is checked before anything else, including the caller's
		// context: the lookup already completed and read the match's status/objective, so
		// the name is known to be occupied by a campaign this create cannot adopt. That
		// fact does not become uncertain because the caller later went away. Nothing was
		// created and a retry re-reads the same conflict, so this is a clean failure.
		if errors.Is(lookupErr, errLookupConflict) {
			steps = append(steps, "Campaign lookup found the name ALREADY IN USE by a campaign this create cannot adopt; nothing was created")
			return nil, fmt.Errorf("meta campaign creation blocked: %w", lookupErr)
		}
		if ctx.Err() != nil {
			// A cancelled/deadlined caller context aborts the create, but "nothing
			// created BY THIS CALL" is not the question this lookup answers — it exists
			// to find a campaign a PRIOR ambiguous attempt may already have created
			// under the same deterministic name. A cancel leaves that unanswered, which
			// is exactly the ambiguous case below, so the name-carrying partial must be
			// retained rather than dropped: returning a bare (nil, err) makes
			// IsOutcomeUnconfirmed false, the dispatcher records a clean failure and
			// releases the claim, and the next retry re-POSTs the same name into an
			// account where Meta enforces no uniqueness — a duplicate PAID campaign.
			// The ad set lookup below already returns partialResult() on cancel for the
			// same reason; this path was the odd one out.
			steps = append(steps, "Campaign lookup was CUT SHORT by a cancelled/expired caller context; cannot confirm the campaign name is absent — verify in Meta Ads Manager before retrying")
			return &CampaignResult{
					Platform:     "meta-ads",
					CampaignName: campaignName,
					AccountID:    accountID,
					MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
					Steps:        steps,
				}, fmt.Errorf("meta campaign creation aborted during name lookup UNCONFIRMED (caller context done; cannot confirm %q is absent, verify in Meta Ads Manager before retrying): %w",
					campaignName, errors.Join(errLookupAmbiguous, lookupErr))
		}
		// EVERY failed lookup is UNCONFIRMED, including a pre-send dial error and a
		// definite 4xx. An earlier version gated this on createOutcomeAmbiguous and it
		// was answering the wrong question: createOutcomeAmbiguous asks "could THIS
		// request have created something", and a GET never creates anything, so on this
		// path it reduces to "was the transport ambiguous" — which is not what the caller
		// needs to know. The lookup exists to establish that the campaign NAME IS ABSENT,
		// so that a prior ambiguous attempt can be adopted instead of duplicated. A dial
		// error establishes nothing about absence. A 4xx establishes nothing about
		// absence. Both leave the question exactly as open as a timeout does.
		//
		// The consequence of getting it wrong is not symmetric. Returning (nil, err) makes
		// IsOutcomeUnconfirmed false, so the dispatcher records a clean failure, releases
		// the retained partial, and the next dispatch POSTs the same deterministic name
		// into an account where Meta enforces no name uniqueness — a duplicate PAID
		// campaign. Over-reporting UNCONFIRMED costs an operator one look in Ads Manager.
		// This is the same reasoning the cancelled-context branch above already applied;
		// that branch was not a special case, it was the general rule arrived at early.
		steps = append(steps, "Campaign lookup FAILED, so its outcome is UNCONFIRMED; cannot confirm the campaign name is absent — verify in Meta Ads Manager before retrying")
		return &CampaignResult{
				Platform:     "meta-ads",
				CampaignName: campaignName,
				AccountID:    accountID,
				MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
				Steps:        steps,
			}, fmt.Errorf("meta campaign lookup UNCONFIRMED (cannot confirm %q is absent; verify in Meta Ads Manager before retrying): %w",
				campaignName, errors.Join(errLookupAmbiguous, lookupErr))
	}

	var campaignID string
	if existingCampaignID != "" {
		campaignID = existingCampaignID
		steps = append(steps, fmt.Sprintf("Campaign already exists by name: %s (not re-created)", campaignID))
	} else {
		var campaignResp createResponse
		err = c.doCreate(ctx, "/"+accountID+"/campaigns", map[string]any{
			"name":                            campaignName,
			"objective":                       objParams.CampaignObjective,
			"status":                          "PAUSED",
			"special_ad_categories":           []string{},
			"is_adset_budget_sharing_enabled": false,
		}, &campaignResp)
		if err != nil {
			// An AMBIGUOUS failure (transport/timeout or a 5xx) can occur AFTER Meta
			// committed the create: the possibly-created PAUSED campaign has the
			// deterministic campaignName, so return a partial result carrying it (like
			// the no-id 2xx case below) plus an UNCONFIRMED step, rather than discarding
			// the name and letting a retry duplicate it. A clear 4xx/validation error
			// means nothing was created, so keep the plain (nil, err). The name is what
			// makes the orphan findable — by an operator in Ads Manager today, and by
			// findCampaignByName above once ReconcileByName is actually set by a caller
			// (nothing in internal/dispatch does yet; the reconciliation is dormant).
			if createOutcomeAmbiguous(err) {
				steps = append(steps, "Campaign creation outcome is UNCONFIRMED (ambiguous response — timeout, server error, or an unfollowed redirect); a PAUSED campaign may exist — verify by name in Meta Ads Manager")
				return &CampaignResult{
					Platform:     "meta-ads",
					CampaignName: campaignName,
					AccountID:    accountID,
					MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
					Steps:        steps,
				}, fmt.Errorf("meta campaign creation UNCONFIRMED (a PAUSED campaign %q may exist): %w", campaignName, err)
			}
			return nil, err
		}
		campaignID = campaignResp.ID
		if campaignID == "" {
			// A 2xx with no id is a malformed success: Meta may have created a PAUSED
			// campaign whose id we couldn't read. Return a partial result carrying the
			// campaign NAME so an orphan is reconcilable by name (not discarded), with an
			// UNCONFIRMED note — reconcilable by an operator in Ads Manager now, and by
			// findCampaignByName above once a caller sets ReconcileByName.
			steps = append(steps, "Campaign creation returned no campaign ID (malformed response); a PAUSED campaign may exist — verify by name in Meta Ads Manager")
			return &CampaignResult{
				Platform:     "meta-ads",
				CampaignName: campaignName,
				AccountID:    accountID,
				MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
				Steps:        steps,
			}, fmt.Errorf("meta campaign creation succeeded but returned no campaign ID (a PAUSED campaign %q may exist)", campaignName)
		}
		// Non-empty is not the same as usable, and this is the ONE campaign id in this
		// client that never passes through numericIDRE: findCampaignByName gates the id
		// it returns, and every consumer of a STORED id gates it again
		// (UpdateCampaignStatus, findAdSetByName), but a freshly created id goes straight
		// into the ad set body and into CampaignResult.CampaignID, which is persisted and
		// spliced into "/{campaignID}/..." paths on later calls. A malformed 2xx carrying
		// "123?fields=x" or a padded "123 " would therefore be stored now and rejected
		// much later, at a call site with no idea a campaign was created. Gate it here,
		// where the campaign name is still in hand.
		//
		// Classified as a malformed SUCCESS, exactly like the empty-id case above: the
		// campaign almost certainly exists (Meta answered 2xx), it just isn't addressable
		// by id — so the name-carrying UNCONFIRMED partial is what makes a retry reconcile
		// by name instead of creating a duplicate paid campaign.
		campaignID = strings.TrimSpace(campaignID)
		if !numericIDRE.MatchString(campaignID) {
			steps = append(steps, fmt.Sprintf("Campaign creation returned an unusable campaign ID %q (malformed response); a PAUSED campaign may exist — verify by name in Meta Ads Manager", campaignID))
			return &CampaignResult{
				Platform:     "meta-ads",
				CampaignName: campaignName,
				AccountID:    accountID,
				MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
				Steps:        steps,
			}, fmt.Errorf("meta campaign creation succeeded but returned a non-numeric campaign ID %q (a PAUSED campaign %q may exist; verify in Meta Ads Manager before retrying)", campaignID, campaignName)
		}
		steps = append(steps, fmt.Sprintf("Campaign created: %s (%s, PAUSED)", campaignID, objectiveLabel(objective)))
	}

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
	// Captured by reference like adCount, so a PARTIAL result returned from a failure
	// after some ads were created still names them (and their image hashes).
	var ads []AdResult
	partialResult := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "meta-ads",
			CampaignName: campaignName,
			CampaignID:   campaignID,
			AdSetName:    adSetName,
			AdSetID:      adSetID,
			AdCount:      adCount,
			AccountID:    accountID,
			MetaURL:      fmt.Sprintf("%s/adsmanager/manage/campaigns?act=%s", c.adsManagerURL, strings.TrimPrefix(accountID, "act_")),
			Steps:        steps,
			Ads:          ads,
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

	// EU DSA advertiser/payer disclosure. Attached only when supplied: Meta rejects an
	// empty string, and a flow that targets no regulated location does not need them.
	// When targeting DOES include a regulated location, Meta blocks publish until both
	// are present ("Please add Advertiser" / "Please add Payer"), so a launch-ready
	// config must set them.
	if in.DSABeneficiary != "" {
		adSetBody["dsa_beneficiary"] = in.DSABeneficiary
	}
	if in.DSAPayor != "" {
		adSetBody["dsa_payor"] = in.DSAPayor
	}

	if in.LifetimeBudget {
		adSetBody["lifetime_budget"] = budgetMinor
	} else {
		adSetBody["daily_budget"] = budgetMinor
	}

	// Reconcile the ad set by name too, same rationale as the campaign lookup above:
	// adSetName is deterministic ("<EventName> - <objective label>", disambiguated by
	// the campaign scope rather than by the name itself), so a prior attempt that got
	// as far as the ad set left one under this campaign that must not be re-POSTed.
	//
	// Gated on existingCampaignID: this lookup is only meaningful when THIS call reused
	// a campaign a PRIOR attempt created. If the campaign was created a few lines above,
	// its id was allocated by Meta just now — no earlier attempt could have parented an
	// ad set to an id that did not exist yet, so the GET can only ever return empty. It
	// would still be a live network call that can fail, and a transient failure there
	// would abandon a freshly created campaign as an orphan for no reconciliation
	// benefit at all. Skip it.
	var existingAdSetID string
	if existingCampaignID != "" {
		var adSetLookupErr error
		existingAdSetID, adSetLookupErr = c.findAdSetByName(ctx, campaignID, adSetName)
		if adSetLookupErr != nil {
			// Same split as the campaign lookup above, and for the same reason. A definite
			// conflict — findAdSetByName enumerated the name and read a match that is not
			// PAUSED — is a confirmed PRESENCE with a stated reason, so it stays a clean
			// failure whatever the caller's context did afterwards.
			//
			// Clean means nil, not a partial. The dispatcher's rule is result==nil releases
			// the claim and ANY non-nil result is retained as UNCONFIRMED (internal/dispatch/
			// meta.go, at the CreateCampaign call) — there is no third shape for "definite
			// failure, but here is some context". Returning partialResult() here therefore
			// reported a stable, re-readable conflict as "verify in Ads Manager", forever:
			// every retry re-reads the same non-PAUSED ad set and re-retains, which is the
			// loop errLookupConflict exists to prevent.
			//
			// Nothing is lost by dropping the partial, because this branch is reachable only
			// under existingCampaignID != "" — the campaign was FOUND BY NAME, not created by
			// this call — and adSetID/adCount are still zero. The partial described a campaign
			// that predates this dispatch entirely.
			if errors.Is(adSetLookupErr, errLookupConflict) {
				return nil, fmt.Errorf("meta ad set lookup found the name already in use under reused campaign %s (found by name, not created by this call; nothing was created): %w", campaignID, adSetLookupErr)
			}
			// Everything else is UNCONFIRMED, including a cancelled context, a pre-send
			// dial error and a definite 4xx. An earlier version reported those as clean
			// failures on the grounds that "the ad set was definitely not looked up" —
			// which is true and beside the point. This lookup exists to establish that the
			// ad set NAME IS ABSENT under a campaign a PRIOR attempt created, and a lookup
			// that never left the process establishes that no better than a timeout does.
			// Report it as failed and the next dispatch POSTs the same deterministic name
			// under the same campaign: a duplicate ad set, spending real budget.
			//
			// These two DO retain the partial: the ad set's absence is genuinely open, so a
			// released claim lets the next dispatch POST the same deterministic ad-set name
			// under the same campaign. They say "reused" rather than "created" for the same
			// reason as the conflict arm — this branch only runs when the campaign was found
			// by name.
			if ctx.Err() != nil {
				return partialResult(), fmt.Errorf("meta ad set lookup UNCONFIRMED (reused campaign %s, PAUSED; caller context done, so ad set %q cannot be confirmed absent; verify in Meta Ads Manager before retrying): %w", campaignID, adSetName, errors.Join(errLookupAmbiguous, adSetLookupErr))
			}
			return partialResult(), fmt.Errorf("meta ad set lookup UNCONFIRMED (reused campaign %s, PAUSED; cannot confirm ad set %q is absent; verify in Meta Ads Manager before retrying): %w", campaignID, adSetName, errors.Join(errLookupAmbiguous, adSetLookupErr))
		}
	}
	if existingAdSetID != "" {
		adSetID = existingAdSetID
		steps = append(steps, fmt.Sprintf("Ad set already exists by name: %s (not re-created)", adSetID))
	} else {
		var adSetResp createResponse
		if err := c.doCreate(ctx, "/"+accountID+"/adsets", adSetBody, &adSetResp); err != nil {
			// The campaign was already created (PAUSED). Return a partial result carrying
			// its id so the caller can identify/clean up the orphan without parsing the
			// error string; auto-deleting here would race a retry that reuses it.
			//
			// An AMBIGUOUS ad-set failure (transport/timeout, a mutating 3xx now surfaced
			// because redirects aren't followed, or a 5xx) can occur AFTER Meta committed
			// the ad set — a definite "failed" instruction would let a retry create a
			// DUPLICATE ad set. Word it UNCONFIRMED (verify before retrying) in that case;
			// a clear 4xx rejection means nothing was created, so keep the plain "failed"
			// wording. Mirrors the campaign and ad/creative create paths. The retained
			// campaign id and deterministic ad-set name are what make the orphan findable:
			// by an operator in Ads Manager today, and by findAdSetByName above once a
			// caller sets ReconcileByName (nothing in internal/dispatch does yet).
			if createOutcomeAmbiguous(err) {
				return partialResult(), fmt.Errorf("meta ad set creation UNCONFIRMED (campaign %s created, PAUSED; an ad set may exist — verify in Meta Ads Manager before retrying): %w", campaignID, err)
			}
			return partialResult(), fmt.Errorf("meta ad set creation failed (campaign %s created, PAUSED): %w", campaignID, err)
		}
		adSetID = adSetResp.ID
		if adSetID == "" {
			// A 2xx with no id is a malformed SUCCESS: Meta may have created the ad set
			// but didn't return a usable id. UNCONFIRMED (verify before retrying), NOT a
			// clean failure — a blind retry could duplicate an ad set Meta already made.
			// Mirrors the campaign/ad no-id and the ad-set error-path handling.
			return partialResult(), fmt.Errorf("meta ad set creation UNCONFIRMED (campaign %s created, PAUSED; Meta returned a 2xx with no ad set ID — an ad set may exist; verify in Meta Ads Manager before retrying)", campaignID)
		}
		// Same gate, same reasoning as the campaign id above: findAdSetByName validates
		// the id it returns, so a freshly created ad set id is the only one that reaches
		// CampaignResult.AdSetID ungated. Treated as a malformed SUCCESS — the ad set
		// exists, it just isn't addressable — so the partial result (which carries the
		// campaign id and the ad set NAME) lets a retry reconcile.
		adSetID = strings.TrimSpace(adSetID)
		if !numericIDRE.MatchString(adSetID) {
			unusable := adSetID
			adSetID = "" // keep the unusable id out of the partial result
			return partialResult(), fmt.Errorf("meta ad set creation UNCONFIRMED (campaign %s created, PAUSED; Meta returned a 2xx with a non-numeric ad set ID %q — an ad set named %q may exist; verify in Meta Ads Manager before retrying)", campaignID, unusable, adSetName)
		}
		budgetLabel := "daily"
		if in.LifetimeBudget {
			budgetLabel = "lifetime"
		}
		// Currency-neutral: Meta interprets the budget in the ad account's currency,
		// which may not be USD, so don't prefix with '$'.
		steps = append(steps, fmt.Sprintf("Ad set created: %s (%.2f %s budget, geo: %s)", adSetID, in.Budget, budgetLabel, strings.Join(geoCountries, ", ")))
	}

	// Step 4: creative + ad per variant (per-variant failures are non-fatal).
	for i, variant := range validVariants {
		utmURL := buildUTMURL(in, i)

		adID, creativeID, imageHash, verr := c.createVariantAd(ctx, in, variant, adSetID, utmURL, i)
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
			// An AMBIGUOUS ad/creative error (transport/timeout, 5xx, or a 2xx with no
			// id — all surfaced by createVariantAd) means Meta MAY already have created
			// the object, so a definite "create it manually" instruction could
			// duplicate it. Record it as UNCONFIRMED (verify before recreating) instead.
			// A clearly non-ambiguous error (a 4xx Meta rejection — nothing created)
			// keeps the definite failure wording. Mirrors how the reddit client words
			// UNCONFIRMED vs FAILED ad outcomes. Per-variant behavior is unchanged:
			// record in Steps and continue.
			if createOutcomeAmbiguous(verr) {
				if creativeID != "" {
					steps = append(steps, fmt.Sprintf("Ad/creative creation outcome UNCONFIRMED for variant %d; it may have been created — verify in Meta Ads Manager before recreating (orphaned creative: %s): %s", i+1, creativeID, scrubURLFromErr(verr, variant.ImageURL, 300)))
				} else {
					steps = append(steps, fmt.Sprintf("Ad/creative creation outcome UNCONFIRMED for variant %d; it may have been created — verify in Meta Ads Manager before recreating: %s", i+1, scrubURLFromErr(verr, variant.ImageURL, 300)))
				}
				continue
			}
			// If the creative was created before the ad failed, surface its id so the
			// orphaned creative is visible (can be cleaned up / reused) rather than
			// silently discarded.
			if creativeID != "" {
				steps = append(steps, fmt.Sprintf("Ad %d failed: %s (orphaned creative: %s)", i+1, scrubURLFromErr(verr, variant.ImageURL, 300), creativeID))
			} else {
				steps = append(steps, fmt.Sprintf("Ad %d failed: %s", i+1, scrubURLFromErr(verr, variant.ImageURL, 300)))
			}
			continue
		}
		adCount++
		ads = append(ads, AdResult{Variant: i + 1, AdID: adID, CreativeID: creativeID, ImageHash: imageHash})
		// Show the SANITIZED display URL in the human-readable step (Steps may be
		// persisted/logged), not the full utmURL — which preserves the caller's
		// original query/fragment and could leak a secret like ?token=... The real
		// ad still uses the full utmURL as its click destination (createVariantAd).
		steps = append(steps, fmt.Sprintf("Ad %d created: %s (creative: %s) → %s", i+1, adID, creativeID, displayMetaUTMURL(in, i)))
	}

	if adCount == 0 && len(in.Variants) > 0 {
		steps = append(steps, "No ads could be created — create them manually in Meta Ads Manager")
	}

	// Success: partialResult() now carries the fully-created campaign, ad set, and
	// ad count (same fields as a bespoke literal); reuse it so success and partial
	// failure return an identically-shaped result.
	return partialResult(), nil
}

// objectStorySpec builds a creative's object_story_spec for a single-image (or,
// with neither image field set, link-only) website-click ad.
//
// It is the ONE place the two image paths converge, and they are exclusive here for
// the same reason validateVariantImage refuses both up front: Meta documents
// link_data.picture as "Specify this field or image_hash but not both."
//
//   - variant.ImageURL set  → link_data.picture, the DOCUMENTED by-URL field. Meta
//     fetches the URL server-side and saves it into the account's image library, so
//     this service never dereferences it and the creative stays a single JSON create.
//   - imageHash non-empty   → link_data.image_hash, the account-scoped hash returned
//     by the /adimages upload the caller already performed.
//   - neither               → the pre-image link-only creative.
//
// It is also the seam for further creative formats: a video or carousel creative
// differs from this one ONLY in this spec (video_data / child_attachments in place
// of link_data), so a new format lands as a sibling builder selected here by the
// resolved asset's kind, leaving createVariantAd's creative/ad POST flow untouched.
func objectStorySpec(pageID, utmURL string, variant AdVariant, imageHash string) map[string]any {
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
	// Exclusive by construction: `picture` wins the branch only when no hash was
	// uploaded, and validateVariantImage has already refused any variant that could
	// reach here with both. Two guards, deliberately: the validator gives the caller a
	// clear pre-spend error, this branch makes the wire body incapable of carrying both
	// even if a future path skipped the validator.
	if img := strings.TrimSpace(variant.ImageURL); img != "" && imageHash == "" {
		// The URL was already validated up front (validateVariantImage), so a rejection
		// here is an upstream/API failure, not a caller error. No upload call is made:
		// `picture` carries the URL on the creative create itself.
		linkData["picture"] = img
	} else if imageHash != "" {
		linkData["image_hash"] = imageHash
	}
	return map[string]any{
		"page_id":   pageID,
		"link_data": linkData,
	}
}

// adImageUploadResponse mirrors the /act_{id}/adimages response: the uploaded
// image(s), each carrying the account-scoped hash a creative references.
//
// ON THE KEY: the reference documents the shape as
// `Map { string: Map { string: Struct { hash, url, ... } } }` but does not say what
// the outer string IS. The official PHP SDK does: it reads
// `images[basename($filename)]`, so Meta keys the map by the BASENAME of the
// filename the upload sent. That contract lives in SDK source rather than in the
// reference, so uploadImage deliberately does NOT derive it — it reads the single
// entry BY VALUE and stays correct whatever Meta names the key.
//
// Reading by value is sound only while there is exactly ONE entry, which one upload
// per call guarantees. uploadImage ENFORCES that count rather than assuming it: see
// the len(out.Images) != 1 guard, which exists because iterating and taking the first
// non-empty hash returned an arbitrary image under Go's randomized map iteration.
type adImageUploadResponse struct {
	Images map[string]struct {
		Hash string `json:"hash"`
		URL  string `json:"url"`
	} `json:"images"`
}

// uploadImage uploads one image's BYTES to the ad account and returns its
// account-scoped image_hash for createVariantAd to attach to a creative.
//
// ON THE TRANSPORT, since this edge has a history in this file and the earlier note
// here was itself imprecise enough to draw two review findings:
//
// The reference for POST /act_<id>/adimages documents exactly TWO create parameters —
// `bytes` ("Image file", typed "Base64 UTF-8 string") and `copy_from`. It documents NO
// multipart file field, and describes no multipart upload at all. So the naive reading
// of that parameter list — "the part must therefore be named `bytes`" — is NOT
// supported by anything Meta publishes; the docs are SILENT on this mechanism rather
// than prescriptive about it.
//
// What settles the shape is the two OFFICIAL SDKs, which disagree with each other and
// are both in production: the Python SDK's FacebookRequest.add_file builds
// `file_key = 'source' + str(self._file_counter)` and uploads under `source0`, while
// the PHP SDK's AdImage.php sets AdImageFields::FILENAME and uploads under `filename`.
// Two vendor SDKs, two different part names, same endpoint — the upload handler is
// LENIENT about the part's field name and treats any file part as the image. No
// particular name is the contract, so the name below is not load-bearing and is not
// evidence of a defect either way. What IS load-bearing is that the part carries a
// FILENAME, since that is what makes Graph treat it as a file upload rather than a
// scalar field — and, per adImageUploadResponse, it is the basename Meta echoes back
// as the response key.
//
// Whether the filename needs a real EXTENSION is undocumented in both directions; no
// authoritative source was found either way, and Meta sniffs content for format. It is
// therefore not asserted here or in the tests.
//
// An earlier revision of the image feature called this edge with a `url` field, which
// is NOT an accepted input (url appears only on the RETURNED image object), so that
// call would have been rejected live after the campaign and ad set already existed;
// PR #144 correctly removed it and switched the by-URL case to link_data.picture. That
// verdict is about the `url` PARAMETER, not about this edge: uploading the bytes is
// the only way to attach an image we hold as stored bytes rather than as a URL. Both
// paths now coexist — see objectStorySpec.
//
// Meta CONTENT-ADDRESSES ad images — identical bytes always yield the same hash and
// a repeat upload is a no-op returning that hash — so this is idempotent and needs
// none of do()'s create-ambiguity/retry machinery: a duplicate upload creates
// nothing. It therefore makes a single attempt and classifies the outcome with the
// SAME error types do() uses, so callers and tests see one vocabulary:
//   - pre-send dial failure → plain error (nothing was uploaded);
//   - non-2xx → *APIError carrying the Graph envelope (or a redacted body snippet);
//   - a 2xx we cannot read/parse, or that names no hash → *transportError (ambiguous).
//
// The bytes go as a multipart FILE part rather than base64 in JSON, to avoid a ~33%
// size inflation on an image that may be several megabytes; contentType, when known,
// labels the part. The bytes themselves are NEVER logged or echoed into an error —
// no length, no checksum, no snippet — so an error from here is safe for a persisted
// Step in the same way scrubURLFromErr makes the by-URL path safe.
func (c *Client) uploadImage(ctx context.Context, image []byte, contentType string) (string, error) {
	if c.creds.AccessToken == "" {
		return "", fmt.Errorf("meta access token is not configured")
	}
	if len(image) == 0 {
		return "", fmt.Errorf("meta image upload called with no bytes")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// The part's field name is NOT the contract — Meta's own SDKs send different ones
	// (`source0` in Python, `filename` in PHP) against this same endpoint, so the
	// handler accepts any file part. Any stable name works; this one is left as-is.
	// The FILENAME is what matters: it makes Graph treat the part as a file upload
	// rather than a scalar field, and Meta echoes its basename back as the response
	// key. uploadImage reads the single entry by value rather than by that key.
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", `form-data; name="source"; filename="creative"`)
	if strings.TrimSpace(contentType) != "" {
		partHeader.Set("Content-Type", contentType)
	}
	part, perr := mw.CreatePart(partHeader)
	if perr != nil {
		return "", fmt.Errorf("build image upload body: %w", perr)
	}
	if _, werr := part.Write(image); werr != nil {
		return "", fmt.Errorf("build image upload body: %w", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		return "", fmt.Errorf("build image upload body: %w", cerr)
	}

	path := "/" + c.account.AccountID + "/adimages"
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, &body)
	if rerr != nil {
		return "", fmt.Errorf("build request: %w", rerr)
	}
	req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, derr := c.httpClient.Do(req)
	if derr != nil {
		// Same split as do(): a failure BEFORE the request left the client means nothing
		// was uploaded (plain error); a mid-flight failure is ambiguous (transportError).
		// The upload being idempotent makes ambiguity harmless, but the caller fails the
		// variant either way.
		if isPreSendDialError(derr) {
			return "", fmt.Errorf("meta API %s %s: %w", http.MethodPost, path, derr)
		}
		return "", &transportError{Method: http.MethodPost, Path: path, Err: derr}
	}
	// One byte past the cap so truncation is detectable, exactly as do() reads.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	status := resp.StatusCode
	_ = resp.Body.Close()

	if status < 200 || status >= 300 {
		apiErr := &APIError{StatusCode: status, Method: http.MethodPost, Path: path}
		var env graphErrorEnvelope
		switch {
		case readErr == nil && json.Unmarshal(raw, &env) == nil && env.Error != nil:
			apiErr.Type = env.Error.Type
			apiErr.Code = env.Error.Code
			apiErr.FBTraceID = env.Error.FBTraceID
			apiErr.Message = env.Error.Message
		case readErr == nil:
			// Non-Graph or malformed error body: surface a redacted, truncated snippet so
			// the reason is not lost. REDACT FIRST — a reflected request could echo the
			// Bearer token (see do()'s matching branch).
			if snippet := strings.TrimSpace(string(raw)); snippet != "" {
				apiErr.Message = truncate(c.redactSecrets(snippet), 300)
			}
		default:
			// Body unread: a missing Code means "we never read it", not "Meta sent none" —
			// mark it so callers do not read a bare status as a clean rejection.
			apiErr.EnvelopeUnreadable = true
			apiErr.Message = fmt.Sprintf("read response body: %v", readErr)
		}
		return "", apiErr
	}
	if readErr != nil {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("read response body: %w", readErr)}
	}
	if int64(len(raw)) > maxResponseBody {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBody)}
	}

	var out adImageUploadResponse
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("decode response: %w", uerr)}
	}
	// ONE upload yields ONE entry, and that invariant is enforced rather than assumed.
	//
	// The earlier revision iterated the map and returned the first non-empty hash. That
	// was two defects in one line. Go RANDOMIZES map iteration order, so a response
	// carrying more than one entry returned an ARBITRARY hash — and this hash becomes
	// link_data.image_hash on a creative that spends money, so the failure mode is the
	// wrong creative on a live paid ad, silently. It was also a workaround for a key
	// this client never derives: Meta keys `images` by the BASENAME of the uploaded
	// filename (the official PHP SDK reads images[basename(filename)]), which is a
	// contract documented only in SDK source, not in the reference.
	//
	// Reading the single entry BY VALUE keeps the client independent of that key — the
	// right call, since the key's contract is not documented — but it is only sound
	// while there is exactly one entry. So the count is checked first and anything else
	// is REFUSED. Failing closed is mandatory here: guessing among entries is what puts
	// an unapproved image on a paid ad.
	if len(out.Images) != 1 {
		return "", &transportError{Method: http.MethodPost, Path: path,
			Err: fmt.Errorf("image upload returned %d image entries, want exactly 1", len(out.Images))}
	}
	for _, img := range out.Images {
		if h := strings.TrimSpace(img.Hash); h != "" {
			return h, nil
		}
	}
	// A 2xx naming no hash is ambiguous the same way a create returning no id is: the
	// upload may have landed but we cannot name it. transportError, so the variant fails
	// rather than attaching an empty image_hash to a creative.
	return "", &transportError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("image upload returned no hash")}
}

// createVariantAd creates the adcreative and ad for one variant, returning the ad
// id, the creative id, and the image_hash attached to the creative ("" for a
// link-only creative OR for a by-URL creative, which carries no hash by
// construction). imageHash is surfaced rather than kept internal so the caller can
// record it in the per-ad result for reconciliation.
//
// The variant's image reaches the creative by ONE of two documented routes, already
// proven exclusive by validateVariantImage:
//   - ImageBytes set → uploaded to /act_<id>/adimages FIRST, and the returned hash
//     becomes link_data.image_hash;
//   - ImageURL set   → no upload at all; the URL is carried on the creative create
//     itself as link_data.picture.
//
// The upload runs BEFORE the creative so a rejected image fails the variant before a
// creative or ad exists. It is idempotent (content-addressed), so a re-dispatch that
// reaches here again re-derives the same hash rather than duplicating the image.
func (c *Client) createVariantAd(ctx context.Context, in CampaignInput, variant AdVariant, adSetID, utmURL string, i int) (adID, creativeID, imageHash string, err error) {
	if len(variant.ImageBytes) > 0 {
		if imageHash, err = c.uploadImage(ctx, variant.ImageBytes, variant.ImageMIME); err != nil {
			// No bytes, size, or checksum in the message — only which variant failed.
			return "", "", "", fmt.Errorf("upload image for variant %d: %w", i+1, err)
		}
	}

	creativeBody := map[string]any{
		"name":              fmt.Sprintf("%s - Variant %d", in.EventName, i+1),
		"object_story_spec": objectStorySpec(c.account.PageID, utmURL, variant, imageHash),
	}
	// Bind the Instagram identity so an ad set requesting an Instagram placement (the
	// default includes Instagram Feed) is publishable. Without it Meta flags "Please add
	// Instagram account" and blocks publish. instagram_user_id is a top-level adcreative
	// field, sibling to object_story_spec — not nested inside it. Sent only when
	// configured so Facebook-only creatives are unchanged.
	if in.InstagramUserID != "" {
		creativeBody["instagram_user_id"] = in.InstagramUserID
	}

	var creativeResp createResponse
	if err = c.doCreate(ctx, "/"+c.account.AccountID+"/adcreatives", creativeBody, &creativeResp); err != nil {
		return "", "", imageHash, err
	}
	if creativeResp.ID == "" {
		// A 2xx with no id is AMBIGUOUS: Meta may have created the creative but we
		// couldn't read its id. Wrap as transportError so the caller classifies it
		// as "may exist" (createOutcomeAmbiguous) rather than a definite failure.
		return "", "", imageHash, &transportError{Method: http.MethodPost, Path: "/" + c.account.AccountID + "/adcreatives", Err: fmt.Errorf("creative creation returned no ID")}
	}

	var adResp createResponse
	if err = c.doCreate(ctx, "/"+c.account.AccountID+"/ads", map[string]any{
		"name":     fmt.Sprintf("%s - Ad %d", in.EventName, i+1),
		"adset_id": adSetID,
		"creative": map[string]any{"creative_id": creativeResp.ID},
		"status":   "PAUSED",
	}, &adResp); err != nil {
		// The creative was already created; return its id alongside the error so
		// the (non-fatal) caller can record the orphaned creative rather than
		// silently discarding it.
		return "", creativeResp.ID, imageHash, err
	}
	if adResp.ID == "" {
		// A 2xx with no id is AMBIGUOUS: Meta may have created the ad but we couldn't
		// read its id. Wrap as transportError so the caller classifies it as "may
		// exist" (createOutcomeAmbiguous) rather than a definite failure.
		return "", creativeResp.ID, imageHash, &transportError{Method: http.MethodPost, Path: "/" + c.account.AccountID + "/ads", Err: fmt.Errorf("ad creation returned no ID")}
	}
	return adResp.ID, creativeResp.ID, imageHash, nil
}

func objectiveKeys() []string {
	// Derive the accepted objectives from objectiveParams (the source of truth for
	// what CreateCampaign maps) and sort for a stable error message, so this list
	// can't drift if an objective is added/removed. 'leads' runs as a website-leads
	// campaign (LINK_CLICKS to the registration URL).
	keys := make([]string, 0, len(objectiveParams))
	for k := range objectiveParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
