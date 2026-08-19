// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// ---------------------------------------------------------------------------
// Campaign creation (MS-2): find-or-create a PAUSED campaign
//
// The Microsoft Advertising REST hierarchy is Campaign -> AdGroup -> Ad. CreateCampaign
// find-or-creates the PAUSED campaign (POST CampaignManagement/v13/Campaigns) and then
// completes the hierarchy under it — a PAUSED ad group and a PAUSED Responsive Search Ad
// (see createAdGroupAndAd in adgroup_ad.go). Everything is created PAUSED so nothing serves
// until a human enables it.
//
// Two Microsoft-specific transport facts drive the contract here:
//
//   - PartialErrors on 200. The create endpoint returns HTTP 200 with a body of
//     {"CampaignIds":[<id-or-null>], "PartialErrors":[...]}. A per-entity failure
//     is reported as a 200 whose CampaignIds slot is null and whose PartialErrors
//     carries the reason — NOT as a non-2xx status. So a "successful" HTTP call can
//     still be a definite rejection, and the body must be inspected, not just the
//     status. This is the inverse of the google-ads :mutate model (non-2xx on error).
//
//   - Duplicate names are REJECTED. Microsoft rejects a create whose campaign name already
//     exists in the account (CampaignServiceCannotCreateDuplicateCampaign / numeric Code 1115).
//     That rejection is what makes a deterministic name a reliable idempotency key: to keep
//     retries at-most-once, CreateCampaign FIRST looks the campaign up by its deterministic
//     name (findCampaignByName) and returns the existing one instead of hitting the duplicate
//     rejection on a re-create; a create that DOES lose the race to the duplicate rejection is
//     reconciled as already-exists (see isDuplicateCampaignPartial), not a clean failure. The
//     lookup is a read (idempotent, retried on 429); the create is a mutation (not retried on 429).
// ---------------------------------------------------------------------------

const (
	// maxBudget caps the daily budget. Microsoft budgets are a plain decimal amount
	// in the ACCOUNT's currency (NO micros conversion — unlike Google Ads), so this is
	// simply an upper sanity bound on caller input, not a unit-overflow guard. Microsoft
	// enforces a currency-dependent minimum server-side and rejects a too-low budget
	// with a PartialError, which surfaces as a definite failure.
	maxBudget = 1_000_000_000.0

	// maxCampaignNameRunes bounds the composed campaign name. Microsoft Advertising
	// limits Campaign.Name to 128 characters; validated in CHARACTERS (runes), not
	// bytes, before any create call so an over-limit name is rejected up front rather
	// than after a paid create attempt.
	maxCampaignNameRunes = 128

	// campaignTypeSearch is the only campaign type this slice creates. Microsoft's
	// Search campaign is the closest analogue to the google-ads SEARCH shell.
	campaignTypeSearch = "Search"

	// budgetTypeDailyStandard spends the DailyBudget evenly across the day. Mirrors the
	// google-ads STANDARD delivery choice for a conservative PAUSED shell.
	budgetTypeDailyStandard = "DailyBudgetStandard"

	// campaignStatusPaused creates the campaign PAUSED so nothing serves until a human
	// enables it. Microsoft's Campaign.Status enum uses "Paused".
	campaignStatusPaused = "Paused"

	// defaultTimeZone is the Campaign.TimeZone sent on create when the caller supplies
	// none. The v13 Campaign object marks TimeZone "This column is deprecated" YET also
	// "Add: Required" — a genuine contradiction in Microsoft's own docs. Because a
	// MISSING required field fails EVERY create while an unnecessary deprecated field is
	// harmless, we send it. PacificTimeUSCanadaTijuana is a canonical, always-valid
	// enum value; the campaign is PAUSED, so the exact zone only matters once a human
	// enables it and can be adjusted then.
	defaultTimeZone = "PacificTimeUSCanadaTijuana"
)

// CampaignInput is the platform-agnostic request to create a Microsoft Advertising
// campaign. CreateCampaign consumes these fields to build the full PAUSED hierarchy
// (campaign + ad group + responsive search ad). Mirrors the google-ads CampaignInput so
// the orchestrator can build one input shape per platform.
type CampaignInput struct {
	// EventName is the human-readable campaign subject, folded into the campaign name.
	// Caller-supplied and otherwise unbounded, so it is sanitized and the composed name
	// is length-capped before any create call.
	EventName string
	// EventSlug is the URL-safe event identifier used to build the UTM click-through params
	// appended to the created ad's final URL (utm_campaign), matching the sibling clients.
	EventSlug string
	// Project is folded into the composed name alongside EventName. It is the canonical
	// attribution key the data pipeline parses out of the campaign name.
	Project string
	// Budget is the campaign DAILY budget in whole units of the ad ACCOUNT's currency.
	// IMPORTANT: this is NOT a USD amount and the client performs NO foreign-exchange
	// conversion and NO micros conversion — Microsoft interprets DailyBudget directly in
	// the account's own currency, so a value of 50 becomes 50 of whatever the account is
	// denominated in. Must be a finite number > 0 and <= maxBudget.
	Budget float64
	// NameSuffix, when non-empty, is appended to the composed campaign name to make it
	// unique+deterministic per logical campaign. Since Microsoft enforces case-insensitive
	// name uniqueness within the account, a stable NameSuffix is what makes
	// findCampaignByName reliable: a retry composes the SAME name and the pre-create
	// lookup returns the existing campaign instead of hitting the duplicate-name rejection.
	NameSuffix string
	// TimeZone is the Microsoft Campaign.TimeZone enum value. Microsoft marks the field
	// deprecated but still "Add: Required", so it is always sent; when empty,
	// defaultTimeZone is used. A caller that knows the account's intended zone can pass a
	// supported enum string.
	TimeZone string
	// RegistrationURL is the landing page the created Ad points to (its FinalUrls). It
	// is REQUIRED to create the Ad: Microsoft rejects a responsive search ad with no final
	// URL. Validated (https/http only, no embedded userinfo) before any create. UTM params
	// for attribution are appended from EventSlug/Project.
	RegistrationURL string
	// Headlines / Descriptions override the auto-composed responsive-search-ad copy. A
	// Microsoft responsive search ad REQUIRES 3-15 unique headlines and 2-4 unique
	// descriptions. Character limits are WIDTH-DEPENDENT: normal copy allows 30 (headline) /
	// 90 (description) final characters; Microsoft documents a reduced 15 / 45 cap "for
	// languages with double-width characters" (CJK, Korean, Japanese, Chinese, or emoji).
	// v13 publishes no per-character weighted formula, so this client conservatively applies
	// the reduced 15 / 45 cap whenever ANY double-width character is present — it never emits
	// an over-length asset (which would fail the ad after its parents were created), at the
	// cost of truncating mixed copy a little short of the theoretical maximum. Each entry must
	// also contain at least one word and no control character (checkAdCopyList rejects ANY
	// control rune — \t/\v/\f/NUL, not just \n/\r). When a caller supplies fewer than the
	// minimum, deterministic placeholders derived from EventName/Project pad the lists up to
	// the minimum (a safe PAUSED default a human edits before enabling); supplying more than
	// the maximum, a duplicate, an over-long, or a whitespace-only (non-empty, word-less) entry
	// is a clean up-front validation error. A genuinely empty "" entry is IGNORED and padded to
	// the minimum, not rejected. Leave both slices empty to auto-compose entirely.
	Headlines    []string
	Descriptions []string
	// Keywords are the positive Search keywords attached to the created ad group (MS-4).
	// WITHOUT AT LEAST ONE, THE CAMPAIGN CAN NEVER SERVE: a Search ad group with no keywords
	// has nothing to match a query against, so the tree is structurally complete and
	// commercially inert. They are still OPTIONAL here — a caller may legitimately create the
	// shell and add keywords in the Bing UI — but the dispatcher refuses to ACTIVATE a
	// campaign whose keywords were never provisioned, so an unkeyworded campaign cannot be
	// turned on while claiming it will serve. See validateKeywords for the per-entry rules;
	// text is capped at 100 characters and the match type must be Exact, Phrase, or Broad.
	Keywords []Keyword
	// CpcBid is the ad group's max cost-per-click, in whole units of the ACCOUNT's currency
	// (NO micros, NO FX conversion — the same unit rule as Budget). ZERO means UNSET, and
	// unset is a supported state rather than a broken one: Microsoft documents that an ad
	// group with no bid takes "the minimum depending on your account's currency", which is a
	// serve-capable floor in the account's own denomination. This client therefore omits the
	// field rather than inventing a default that could not be currency-correct across every
	// account. A non-zero value must be within [minCpcBid, maxCpcBid].
	CpcBid float64
	// GeoTargets are ISO 3166-1 alpha-2 country codes the campaign should serve in
	// (LFXV2-3279), attached as CAMPAIGN-level location criteria. Optional at this layer: an
	// empty list creates the campaign with NO location criteria, which is what this client
	// did before geo targeting existed and means Microsoft serves it EVERYWHERE.
	//
	// That default is why an unresolvable code is a HARD FAILURE rather than a silent drop.
	// Microsoft takes numeric LocationIds, not ISO codes, so each code is resolved against
	// Microsoft's own geographical-locations file at create time; if any code cannot be
	// resolved, CreateCampaign REFUSES before its first mutating call. Dropping it instead
	// would produce a campaign that spends globally while reporting success — the exact
	// defect this ticket fixes. See geo.go.
	GeoTargets []string
}

// CampaignResult reports what CreateCampaign created (or found). The campaign NAME
// matters on an ambiguous failure BEFORE an id is known: the name is deterministic,
// so a caller reconciling a possibly-created campaign looks it up by CampaignName.
type CampaignResult struct {
	Platform     string `json:"platform"`
	AccountLabel string `json:"accountLabel,omitempty"`
	// AccountID is the ad account the campaign was CREATED under. The dispatcher's
	// microsoftCreationAccountID reads it to detect a later read/toggle resolving to a
	// DIFFERENT account (UpdateMicrosoftAds can re-point a project's connection), and treats
	// an absent one as "unknown, proceed" so rows written before this field existed keep
	// working. Omitting it on any result path would silently disable that check — every path
	// here derives from namePartial, which sets it.
	AccountID    string `json:"accountId,omitempty"`
	CampaignName string `json:"campaignName"`
	CampaignID   string `json:"campaignId"`
	// AdGroupName / AdGroupID identify the ad group created (or found) under the
	// campaign. AdGroupName is deterministic, so an ambiguous ad-group failure BEFORE an
	// id is known is reconcilable by name (scoped to the campaign).
	AdGroupName string `json:"adGroupName,omitempty"`
	AdGroupID   string `json:"adGroupId,omitempty"`
	// AdID identifies the Responsive Search Ad created under the ad group.
	AdID string `json:"adId,omitempty"`
	// KeywordIDs are the ids of the keywords THIS RUN attached to the ad group by the MS-4
	// targeting step. Empty does NOT mean the ad group has no keywords upstream; it means
	// this run parsed no ids, which happens when no keyword input was supplied, when the
	// step failed before any id could be parsed, or when EVERY supplied keyword was already
	// attached and Microsoft refused it as a duplicate (a duplicate entry returns a null id
	// slot, and this client calls no keyword read with which to resolve it — v13 does expose
	// one, see createKeywords). The first two are
	// distinguished from each other by whether an error accompanied the result; the third is
	// identifiable by its Steps entry and carries no error.
	//
	// CONSEQUENCE for the dispatcher's toggle guard, which refuses ACTIVATE when this is
	// empty: on the all-duplicate path it refuses a campaign whose keywords do exist
	// upstream. That is deliberate — the ids needed to enable those Paused keywords are
	// precisely what this run could not learn, so activating would report success while
	// leaving every keyword Paused. Refusing is the honest answer, and reconciliation
	// (LFXV2-2665) is what resolves it.
	//
	// The tree does NOT deadlock on the case that matters, though: an ad group that exists
	// with NO keywords is re-keyworded on the next run (they are posted, not skipped), so the
	// ids land and ACTIVATE succeeds.
	KeywordIDs []string `json:"keywordIds,omitempty"`
	// GeoCriterionIDs are the ids of the CAMPAIGN-level location criteria THIS RUN attached
	// (LFXV2-3279). Empty means the campaign carries no geo targeting from this run, which —
	// unlike an empty KeywordIDs — means Microsoft will serve it EVERYWHERE once enabled.
	//
	// Unlike keywords, these are NOT re-attached on a reused campaign: this client calls no
	// criterion read, so on a retry that finds an existing campaign the ids are unknown to
	// this run and the field is empty even though the targeting is present upstream. The
	// field therefore reports what this RUN did, not the campaign's total targeting.
	GeoCriterionIDs []string `json:"geoCriterionIds,omitempty"`
	// AlreadyExisted is true ONLY when this run created NOTHING — i.e. the campaign, the ad
	// group, AND the ad were all matched as pre-existing (by name / by destination) and no
	// create was issued at any level. If ANY level was created this run, it is false, even
	// when the campaign itself was reused. So true means "the entire tree already existed".
	// (At the campaign level, a reused campaign is one found by findCampaignByName OR one
	// reconciled by the duplicate-name self-heal after losing a create race.)
	AlreadyExisted  bool     `json:"alreadyExisted,omitempty"`
	MicrosoftAdsURL string   `json:"microsoftAdsUrl"`
	Steps           []string `json:"steps"`
}

// msCampaign is one Campaign in the POST CampaignManagement/v13/Campaigns body. Only
// the fields required for a PAUSED Search shell are set. DailyBudget is a plain decimal
// in the account currency (no micros). CampaignType/BudgetType/Status use Microsoft's
// string enums. TimeZone is SENT: the v13 Campaign object marks it deprecated but ALSO
// "Add: Required", so it must be present or the create is rejected (see defaultTimeZone).
type msCampaign struct {
	Name         string  `json:"Name"`
	CampaignType string  `json:"CampaignType"`
	BudgetType   string  `json:"BudgetType"`
	DailyBudget  float64 `json:"DailyBudget"`
	Status       string  `json:"Status"`
	TimeZone     string  `json:"TimeZone"`
	// Languages and other targeting are intentionally omitted for the PAUSED shell;
	// they are set in a later slice.
}

// createCampaignsRequest is the POST /Campaigns body. The v13 AddCampaigns operation
// REQUIRES AccountId at the top level (a sibling to Campaigns, NOT only the
// CustomerAccountId header) — omitting it rejects every create with
// CampaignServiceInvalidAccountId. Microsoft takes an ARRAY of campaigns even when
// creating one, so a single-element slice is sent. AccountId is a numeric string
// (json.Number) matching the account id.
type createCampaignsRequest struct {
	AccountId json.Number  `json:"AccountId"`
	Campaigns []msCampaign `json:"Campaigns"`
}

// createCampaignsResponse is the (subset of the) 200 response. CampaignIds is
// index-aligned with the request Campaigns; a slot is null when that entity failed,
// and the reason is in PartialErrors. Ids are int64 in the wire form; captured as
// json.Number so a null slot is distinguishable from a zero id.
//
// Both arrays are BOUNDED slice types so a malformed up-to-8-MiB response packed with
// null/empty entries can't expand into millions of elements + tens of MB of allocations per
// create (an OOM risk under concurrency): only the first few entries are retained, which is
// all this single-campaign create ever needs (it reads CampaignIds[0] and whether ANY
// PartialError is present). Mirrors boundedErrorItems / the parseErrorCodes decode.
type createCampaignsResponse struct {
	CampaignIds   boundedNumberIDs  `json:"CampaignIds"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
}

// boundedNumberIDs is a []*json.Number that, during UnmarshalJSON, streams the whole JSON
// array (so it never truncates a large valid body) but RETAINS only the first
// maxDecodedErrorItems elements — later elements are decoded into a scratch and dropped. A
// create sends ONE campaign, so only the first id is ever meaningful; this bounds memory to
// O(1) in the response size regardless of a malformed null-padded body.
type boundedNumberIDs []*json.Number

func (b *boundedNumberIDs) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // JSON null
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a JSON array for campaign ids")
	}
	for dec.More() {
		var n *json.Number
		if err := dec.Decode(&n); err != nil {
			return err
		}
		if len(*b) < maxDecodedErrorItems {
			*b = append(*b, n)
		}
	}
	return nil
}

// boundedKeywordIDs is boundedNumberIDs with a bound sized for KEYWORDS rather than campaigns.
//
// The distinction is load-bearing. boundedNumberIDs retains maxDecodedErrorItems (16), which is
// an ERROR-ARRAY bound: a create sends one campaign, so one id is all that can matter, and 16 is
// generous. AddKeywords sends up to maxKeywords (60), and every id is meaningful — it is what
// the status cascade enables on ACTIVATE. Decoding a 60-keyword response through the 16-item
// bound silently retained the first 16, so activation enabled 16 keywords and left the other 44
// Paused on a campaign the operator believes is fully live.
//
// The streaming shape is kept for the same reason the original has it: a malformed null-padded
// body must not materialise in full. Only the retention limit differs.
type boundedKeywordIDs []*json.Number

func (b *boundedKeywordIDs) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // JSON null
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a JSON array for keyword ids")
	}
	for dec.More() {
		var n *json.Number
		if err := dec.Decode(&n); err != nil {
			return err
		}
		if len(*b) < maxKeywords {
			*b = append(*b, n)
		}
	}
	return nil
}

// queryCampaignsRequest is the POST /Campaigns/QueryByAccountId body used by
// findCampaignByName. The v13 GetCampaignsByAccountId REST operation is a POST with a
// JSON body (NOT a GET) carrying the required AccountId and the CampaignType to scope
// the read; ReturnAdditionalFields is omitted (the default fields include Id/Name).
type queryCampaignsRequest struct {
	AccountId    json.Number `json:"AccountId"`
	CampaignType string      `json:"CampaignType"`
}

// CreateCampaign find-or-creates a PAUSED Microsoft Advertising Search campaign AND then
// completes the Campaign -> AdGroup -> Ad hierarchy under it, returning a usable paused
// campaign rather than an empty shell.
//
// Microsoft enforces that Campaign.Name is UNIQUE among the account's active/paused
// campaigns, using a CASE-INSENSITIVE comparison. That uniqueness is the idempotency
// key here (there is no client-supplied idempotency token): CreateCampaign FIRST looks
// the campaign up by its deterministic name (a read, retried on 429) and reuses the
// existing one without creating a second. Reuse of the campaign ALONE does NOT set
// AlreadyExisted — the call continues into the ad-group/ad steps, and result.AlreadyExisted
// is true ONLY when the ENTIRE tree (campaign AND ad group AND ad) was found pre-existing
// and nothing was created at any level (see CampaignResult.AlreadyExisted).
//
// Otherwise it POSTs the campaign. Because the create reports per-entity failure as
// PartialErrors on a 200, every outcome is classified by whether it may have committed:
//   - A DUPLICATE-name PartialError (CampaignServiceCannotCreateDuplicateCampaign) means
//     a campaign with that name already exists — from a prior attempt or a race after
//     the pre-check — so it is surfaced as already-exists (reconcile by name), NOT a
//     clean failure.
//   - Any other definite PartialError on a 200 (id slot null) means the campaign was NOT
//     created — a clean failure.
//   - A 200 with a valid id is a clean success.
//   - A 200 that is malformed (no id, no PartialError), or an ambiguous transport/5xx/
//     mutating-429 error, is reported UNCONFIRMED: the campaign MAY exist, so the
//     caller must reconcile by name before retrying rather than blind-creating.
//   - A definite 4xx (Microsoft rejected the request outright) is a clean failure.
func (c *Client) CreateCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// Require BOTH attribution fields, validated on the SANITIZED value (a
	// delimiter-only value like "|||" passes a raw TrimSpace check yet sanitizes to
	// nothing, which would drop the segment from the composed name). Mirrors the
	// google-ads client.
	if sanitizeNamePart(in.Project) == "" {
		return nil, fmt.Errorf("microsoft-ads campaign requires a non-empty Project")
	}
	if sanitizeNamePart(in.EventName) == "" {
		return nil, fmt.Errorf("microsoft-ads campaign requires a non-empty EventName")
	}
	// Reject NaN/Inf explicitly (NaN passes every ordered comparison), and reject a
	// non-positive budget. No micros rounding — Microsoft takes the decimal directly —
	// but a sub-cent budget is still meaningless, so require > 0.
	if math.IsNaN(in.Budget) || math.IsInf(in.Budget, 0) {
		return nil, fmt.Errorf("microsoft-ads campaign budget must be a finite number, got %v", in.Budget)
	}
	if in.Budget <= 0 {
		return nil, fmt.Errorf("microsoft-ads campaign budget must be > 0, got %.2f", in.Budget)
	}
	if in.Budget > maxBudget {
		return nil, fmt.Errorf("microsoft-ads campaign budget %.2f exceeds the maximum %.0f", in.Budget, maxBudget)
	}

	campaignName := composeName(in)
	if err := validateEntityName("campaign", campaignName, utf8.RuneCountInString(campaignName), maxCampaignNameRunes, "characters"); err != nil {
		return nil, err
	}

	// Validate the ad destination URL up front, BEFORE the campaign is created. It becomes
	// the Ad's FinalUrls in a later step; deferring the check until then would let a bad
	// URL fail only AFTER a PAUSED campaign (and possibly ad group) already exists,
	// orphaning them. This is pure input validation with no side effects, so a bad URL is a
	// clean (nil, err) failure — nothing has been created yet.
	if err := validateAdURL(in.RegistrationURL); err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign requires a valid ad destination URL: %w", err)
	}
	// The ad's FinalUrls is the registration URL WITH the LFX utm_* params appended, and
	// Microsoft caps FinalUrls at maxFinalURLRunes. Validate the fully COMPOSED URL length up
	// front: a raw URL near the limit passes validateAdURL but the longer composed URL would
	// be rejected only at AddAds — after the campaign/ad group exist, the exact orphaning the
	// up-front checks prevent.
	finalURL := buildAdFinalURL(in)
	if n := utf8.RuneCountInString(finalURL); n > maxFinalURLRunes {
		return nil, fmt.Errorf("microsoft-ads composed ad final URL is %d characters, exceeding the %d limit (shorten the registration URL)", n, maxFinalURLRunes)
	}
	// Microsoft also derives the ad's DISPLAY domain from the FinalUrls host and caps it at
	// maxDisplayDomainRunes. A host longer than that passes the FinalUrls length check above
	// but is rejected only at AddAds, orphaning the PAUSED campaign/ad group — so reject it up
	// front too. Count the full host AUTHORITY (hostname + a non-default port), not just
	// Hostname(): Hostname() drops the port, so a hostname just under the cap plus e.g. :8443
	// could slip past here and be rejected at AddAds. A redundant default port (:80/:443) is
	// stripped so it never counts against an otherwise-valid host. A parse failure here is a
	// near-impossibility (validateAdURL already accepted the raw URL and buildAdFinalURL only
	// re-encoded the query), but fail CLOSED rather than silently skip the display-domain cap:
	// a skipped check is the exact orphan-after-AddAds risk this block exists to prevent.
	u, perr := url.Parse(finalURL)
	if perr != nil {
		return nil, fmt.Errorf("microsoft-ads could not parse the composed ad final URL to validate its display domain: %w", perr)
	}
	{
		// Decode the host to its Unicode (U-label) form BEFORE the width/length check. finalURL
		// is buildAdFinalURL's u.String() wire form, so a caller-supplied punycode host arrives as
		// its ASCII `xn--` A-label: hasDoubleWidth would never fire on it and the rune count would
		// measure the ASCII form, letting a wide host clear the 67-rune cap only to hit Microsoft's
		// decoded wide-host rule (33 runes) at AddAds — the exact orphaning this check prevents.
		// Conversely a short CJK host inflated by punycode could be false-rejected. idna ToUnicode
		// decodes `xn--` back to CJK and is a no-op for a plain ASCII or already-percent-decoded
		// host. A decode FAILURE means the host is not a valid IDNA label, so reject it here
		// rather than measuring the malformed A-label: a short-but-invalid `xn--` host would
		// otherwise clear this cap and only fail at AddAds, orphaning the PAUSED campaign and ad
		// group this whole block exists to prevent. Fail CLOSED, as with the parse failure above.
		host := u.Hostname()
		// An IPv6 literal is NOT an IDNA label — ToUnicode fails on it — so skip the decode
		// entirely rather than fail closed, mirroring canonicalFinalURL's isIPv6 guard.
		// validateAdURL still accepts IPv6 destinations, so rejecting one here would break a
		// previously-valid URL with a misleading "not a valid IDNA label" error. Hostname()
		// already strips the surrounding brackets, leaving the colons that identify it.
		if !strings.Contains(host, ":") {
			uni, ierr := idna.Lookup.ToUnicode(host)
			if ierr != nil {
				return nil, fmt.Errorf("microsoft-ads composed ad final URL host %q is not a valid IDNA label: %w", host, ierr)
			}
			if uni != "" {
				host = uni
			}
		}
		authority := authorityForWidth(u, host)
		limit := maxDisplayDomainRunes
		if hasDoubleWidth(authority) {
			limit = maxDisplayDomainRunesWide
		}
		if n := utf8.RuneCountInString(authority); n > limit {
			return nil, fmt.Errorf("microsoft-ads ad display domain %q is %d characters, exceeding the %d limit (use a shorter registration URL host)", truncate(authority, maxErrorBodyChars), n, limit)
		}
	}
	// Validate caller-supplied ad copy up front too (over-count / over-long headlines or
	// descriptions), so a bad copy input fails cleanly before the campaign is created rather
	// than at the paid ad create. composeAdCopy pads short lists to the required minimum.
	if err := validateAdCopy(in); err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign ad copy invalid: %w", err)
	}
	// Validate the TARGETING inputs up front too, for the same reason as the URL and the copy:
	// keywords are attached only at the LAST step, so a bad keyword or bid discovered there
	// would fail after the campaign, ad group and ad already exist — orphaning a PAUSED tree
	// over input that was invalid before anything was sent. Both are pure validation with no
	// side effects, so a bad value is a clean (nil, err).
	keywords, err := validateKeywords(in.Keywords)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign keywords invalid: %w", err)
	}
	cpcBid, cpcBidSet, err := validateCpcBid(in.CpcBid)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign bid invalid: %w", err)
	}
	// Geo codes are shape-checked here (pure, offline) and RESOLVED to numeric LocationIds
	// below, before the first mutating call. Splitting the two is deliberate: this half needs
	// no network, so an unsupported code fails with nothing sent at all.
	geoCodes, err := validateGeoTargets(in.GeoTargets)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign geo targets invalid: %w", err)
	}

	var steps []string
	microsoftAdsURL := "https://ads.microsoft.com/campaign/vnext/campaigns?aid=" + c.account.AccountID

	namePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:        "microsoft-ads",
			AccountLabel:    c.account.Label,
			AccountID:       c.account.AccountID,
			CampaignName:    campaignName,
			MicrosoftAdsURL: microsoftAdsURL,
			Steps:           steps,
		}
	}

	// If the caller's context is ALREADY cancelled/expired, nothing has been sent —
	// return a clean (nil, err) rather than firing a request that doRequest would
	// classify as an ambiguous transportError.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("microsoft-ads campaign creation aborted before any request (context already done): %w", ctxErr)
	}

	// Resolve the geo codes to numeric Microsoft LocationIds BEFORE anything is created.
	//
	// This is a NETWORK call (it may download Microsoft's geographical-locations file), so it
	// cannot live in the pure-validation prologue above — but it must still happen before the
	// first MUTATING call, and that ordering is the entire safety property of this ticket.
	// Location criteria can only be attached AFTER the campaign exists; if resolution ran at
	// that point instead, a failure would leave a campaign with NO location criteria, which
	// Microsoft serves EVERYWHERE. The campaign is created paused, but a paused untargeted
	// campaign is one human click away from spending globally, and the operator enabling it
	// has no signal that its targeting silently went missing.
	//
	// Refusing here costs nothing: the lookup and the create below never run, so there is no
	// orphaned campaign to reconcile and a clean (nil, err) is honest about it.
	geoLocationIDs, err := c.resolveGeoTargets(ctx, geoCodes)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign geo targeting could not be resolved (nothing was created): %w", err)
	}

	// Step 1: idempotency lookup by the deterministic (case-insensitively unique) name,
	// so a retry returns the existing campaign instead of hitting the duplicate-name
	// rejection on the create.
	existingID, err := c.findCampaignByName(ctx, campaignName)
	if err != nil {
		// A CALLER context cancellation/deadline is a clean ABORT: the lookup is a read that
		// creates nothing, and the create step below never runs, so nothing exists to
		// reconcile. Return (nil, err) — matching the pre-send guard above — rather than a
		// reconcile-partial that would tell the caller to "verify before retrying" after a
		// plain cancel.
		//
		// Gate on ctx.Err() (the CALLER's context), NOT errors.Is(err, DeadlineExceeded): the
		// client wraps each attempt in its own context.WithTimeout (client.go), so a
		// per-attempt timeout surfaces DeadlineExceeded even while the caller's ctx is still
		// live. That is a FAILED lookup, not a caller abort — it must fall through to the
		// UNCONFIRMED branch (we can't confirm the campaign is absent), never be reported as
		// "nothing created".
		if ctx.Err() != nil {
			return nil, fmt.Errorf("microsoft-ads campaign creation aborted during name lookup (caller context done; nothing created): %w", err)
		}
		// Any OTHER lookup failure: we have NOT created anything, but we also can't
		// confirm the campaign is absent — a blind create might duplicate a campaign a
		// prior attempt made. Report UNCONFIRMED so the caller reconciles by name.
		return namePartial(), fmt.Errorf("microsoft-ads campaign lookup failed (cannot confirm %q is absent; verify in Microsoft Advertising before retrying): %w", campaignName, err)
	}
	var (
		campaignID     string
		alreadyExisted bool
	)
	if existingID != "" {
		steps = append(steps, fmt.Sprintf("Campaign already exists by name: %s (not re-created)", existingID))
		campaignID = existingID
		alreadyExisted = true
	} else {
		// Step 2: create the campaign (PAUSED). Non-idempotent — NOT retried on 429.
		timeZone := in.TimeZone
		if timeZone == "" {
			timeZone = defaultTimeZone
		}
		req := createCampaignsRequest{
			AccountId: json.Number(c.account.AccountID),
			Campaigns: []msCampaign{{
				Name:         campaignName,
				CampaignType: campaignTypeSearch,
				BudgetType:   budgetTypeDailyStandard,
				DailyBudget:  in.Budget,
				Status:       campaignStatusPaused,
				TimeZone:     timeZone,
			}},
		}
		respBody, err := c.doRequest(ctx, http.MethodPost, "Campaigns", req, false)
		if err != nil {
			switch {
			case createOutcomeAmbiguous(err):
				return namePartial(), fmt.Errorf("microsoft-ads campaign creation UNCONFIRMED (%q may exist — verify in Microsoft Advertising before retrying): %w", campaignName, err)
			default:
				return nil, fmt.Errorf("microsoft-ads campaign creation failed: %w", err)
			}
		}
		campaignID, err = firstCampaignID(respBody)
		if err != nil {
			if isDuplicateCampaignNameErr(err) {
				// A duplicate-name PartialError: a campaign with this name already exists (a
				// prior attempt, or a race between the pre-check lookup and this create).
				// SELF-HEAL by re-looking it up by name and treating it as already-exists —
				// the deterministic name is unique, so the re-lookup finds the winner. This
				// mirrors the ad-group path (findOrCreateAdGroup on a 1214), so a duplicate
				// race resolves to a usable id instead of forcing the caller to reconcile.
				reResolvedID, ferr := c.findCampaignByName(ctx, campaignName)
				if ferr != nil || reResolvedID == "" {
					// Re-lookup failed or returned no id: the campaign exists but we can't
					// confirm its id, so surface UNCONFIRMED rather than a clean failure.
					// Surface the RE-LOOKUP cause (ferr) when it errored — the original
					// duplicate error only says "a duplicate exists", it hides WHY the
					// reconciliation couldn't resolve the id (a 500, auth failure, timeout).
					if ferr != nil {
						return namePartial(), fmt.Errorf("microsoft-ads campaign %q already exists (duplicate name) but the reconciliation lookup failed (%v); verify in Microsoft Advertising before retrying: %w", campaignName, ferr, err)
					}
					return namePartial(), fmt.Errorf("microsoft-ads campaign %q already exists (duplicate name) but could not be re-resolved (reconciliation lookup returned no id); verify in Microsoft Advertising before retrying: %w", campaignName, err)
				}
				steps = append(steps, fmt.Sprintf("Campaign already exists by name (duplicate-name race reconciled): %s", reResolvedID))
				campaignID = reResolvedID
				alreadyExisted = true
			} else if errors.Is(err, errPartialFailure) {
				// A 200 with a null id slot + PartialErrors is a DEFINITE rejection: the
				// campaign was not created. Clean failure, not UNCONFIRMED.
				return nil, fmt.Errorf("microsoft-ads campaign creation rejected: %w", err)
			} else {
				// A 200 with no id and no PartialError is a malformed success: the campaign
				// MAY have been created. UNCONFIRMED.
				return namePartial(), fmt.Errorf("microsoft-ads campaign creation UNCONFIRMED (%q may exist — verify in Microsoft Advertising before retrying): %w", campaignName, err)
			}
		} else {
			steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, Search, %.2f/day daily budget in account currency)", campaignID, in.Budget))
		}
	}

	// campaignPartial carries the campaign id + name (and accumulates ad-group/ad ids as
	// they land) so an ambiguous ad-group/ad failure leaves the whole tree reconcilable.
	// It deliberately does NOT set AlreadyExisted: a partial is returned on a failed or
	// UNCONFIRMED ad-group/ad step, where this run may have created (or attempted) a lower
	// level even though the campaign was reused — so "created nothing" is not true. Only the
	// clean success path sets AlreadyExisted, and only when ALL three levels pre-existed.
	// geoCriterionIDs accumulates the ids of the location criteria attached below, so a later
	// ad-group/ad failure still reports the targeting this run put on the campaign.
	var geoCriterionIDs []string
	campaignPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignID = campaignID
		r.GeoCriterionIDs = geoCriterionIDs
		return r
	}

	// Step 2.5: attach the CAMPAIGN-level location criteria, before the ad-group cascade.
	//
	// Location criteria belong to the campaign, so this is the earliest point they CAN be
	// attached — and attaching them first is deliberate: it narrows the window in which a
	// campaign exists with no targeting. The ids were resolved before the campaign was
	// created, so this step can no longer fail for an unresolvable code; only the POST itself
	// can fail, and that failure is reported rather than swallowed.
	//
	// A geo failure is NOT downgraded to a warning. An untargeted campaign is the harm this
	// ticket exists to prevent, so the caller gets an error and a partial carrying the
	// campaign id, which is what a reconciliation needs to either finish or delete the tree.
	//
	// ON A REUSED CAMPAIGN THE EXISTING CRITERIA ARE READ FIRST, because neither guess is safe.
	// Re-posting blindly DUPLICATES every location: AddCampaignCriterions publishes no
	// duplicate refusal, unlike AddKeywords (1517/1542), and the dispatcher retries by design
	// (NameSuffix = brief.ID composes the same name so the lookup reuses the campaign), so each
	// retry would widen the criterion list on a live paid campaign. Skipping blindly is worse:
	// a run whose attach was REJECTED leaves a campaign with NO criteria, and the retry would
	// then finish the cascade and report SUCCESS for a campaign that serves everywhere — the
	// exact harm this ticket exists to prevent, reintroduced through the back door.
	//
	// POST /CampaignCriterions/QueryByIds distinguishes the two, and only the locations that
	// are genuinely missing are attached. A read FAILURE is propagated rather than treated as
	// "no criteria": "we could not check" must not collapse into the re-attach path.
	if len(geoLocationIDs) > 0 {
		wanted := geoLocationIDs
		if alreadyExisted {
			existing, rerr := c.existingLocationIDs(ctx, campaignID)
			if rerr != nil {
				return campaignPartial(), fmt.Errorf("microsoft-ads could not read the existing location criteria of reused campaign %s, so its geo targeting cannot be confirmed or safely completed (do not enable it until verified): %w", campaignID, rerr)
			}
			missing := make([]string, 0, len(geoLocationIDs))
			for _, id := range geoLocationIDs {
				if _, have := existing[id]; !have {
					missing = append(missing, id)
				}
			}
			wanted = missing
			if len(wanted) == 0 {
				steps = append(steps, fmt.Sprintf("Geo targeting already present on reused campaign %s (%s) — not re-attached", campaignID, strings.Join(geoCodes, ", ")))
			}
		}
		if len(wanted) > 0 {
			geoIDs, gerr := c.createCampaignCriterions(ctx, campaignID, wanted)
			if gerr != nil {
				partial := campaignPartial()
				partial.GeoCriterionIDs = geoIDs
				switch {
				case createOutcomeAmbiguous(gerr) || errors.Is(gerr, errNoID):
					return partial, fmt.Errorf("microsoft-ads geo targeting UNCONFIRMED (campaign %s exists; its location criteria may or may not have been attached — verify its targeting in Microsoft Advertising before enabling or retrying): %w", campaignID, gerr)
				case errors.Is(gerr, context.Canceled) || errors.Is(gerr, context.DeadlineExceeded):
					return partial, fmt.Errorf("microsoft-ads geo targeting aborted (campaign %s exists with incomplete location criteria and would serve too widely if enabled; context done): %w", campaignID, gerr)
				case errors.Is(gerr, errPartialFailure) && len(geoIDs) > 0:
					// Some criteria DID attach. Saying "NOT geo-targeted" here would be false and
					// would send an operator to delete targeting that exists; the campaign is
					// targeted to a SUBSET, which still must not be enabled as-is.
					return partial, fmt.Errorf("microsoft-ads geo targeting PARTIALLY attached (campaign %s carries %d of %d requested location criteria and would serve too widely if enabled; reconcile the missing ones before enabling): %w", campaignID, len(geoIDs), len(wanted), gerr)
				default:
					return partial, fmt.Errorf("microsoft-ads geo targeting rejected (campaign %s exists but is NOT geo-targeted and would serve everywhere if enabled; do not enable it until its targeting is fixed): %w", campaignID, gerr)
				}
			}
			geoCriterionIDs = geoIDs
			steps = append(steps, fmt.Sprintf("Geo targeting attached: %d location criteria (%s)", len(geoIDs), strings.Join(geoCodes, ", ")))
		}
	}

	// Steps 3-4: complete the Campaign -> AdGroup -> Ad hierarchy (all PAUSED) so the
	// result is a usable paused campaign rather than an empty shell.
	// The VALIDATED targeting values are threaded through rather than re-derived downstream:
	// re-running validateKeywords/validateCpcBid inside the ad-group step would let the
	// up-front check and the sent value drift apart, and the up-front check is the one that
	// guarantees nothing is orphaned.
	return c.createAdGroupAndAd(ctx, in, campaignID, alreadyExisted, &steps, campaignPartial,
		targeting{keywords: keywords, cpcBid: cpcBid, cpcBidSet: cpcBidSet})
}

// findCampaignByName returns the id of the campaign whose Name matches name, or "" if
// none matches. It POSTs CampaignManagement/v13/Campaigns/QueryByAccountId with the
// account id + campaign type in the body — the v13 GetCampaignsByAccountId REST
// operation is a POST-with-body, NOT a GET. It is a READ (idempotent), retried on 429.
//
// The match is CASE-INSENSITIVE, matching Microsoft's own uniqueness comparison: a
// campaign the service would reject as a duplicate of `name` must be found here so a
// retry reuses it rather than hitting the duplicate-name rejection. composeName
// produces a deterministic name, so this can't return an unrelated campaign.
//
// SCOPE: the lookup is filtered to Search campaigns (campaignTypeSearch) — the only type this
// client creates — which optimizes the common same-type retry. Microsoft's name uniqueness is
// ACCOUNT-WIDE (across types), so a same-named campaign of a DIFFERENT type is not found here;
// that rare cross-type collision is not silently created, though — the subsequent create hits
// the account-wide duplicate rejection (code 1115), which CreateCampaign reconciles as
// already-exists. So the Search-scoped find-first is a fast path, and the create-time 1115
// handling is the correctness backstop for the cross-type case.
//
// QueryByAccountId returns the FULL set of campaigns of the requested type for the account in
// one response (not cursor-paged), so the single-shot read can't miss an existing Search
// campaign to a pagination boundary. The 8 MiB response cap (maxResponseBytes) is the only
// bound; an account with an implausibly large campaign count would fail the read and be
// reported UNCONFIRMED rather than silently skipping the match.
func (c *Client) findCampaignByName(ctx context.Context, name string) (string, error) {
	req := queryCampaignsRequest{
		AccountId:    json.Number(c.account.AccountID),
		CampaignType: campaignTypeSearch,
	}
	body, err := c.doRequest(ctx, http.MethodPost, "Campaigns/QueryByAccountId", req, true)
	if err != nil {
		return "", err
	}
	// STREAM the Campaigns array comparing names as we go, rather than Unmarshaling the whole
	// account campaign set into memory: a malformed (up to 8 MiB) body packed with `{}` entries
	// would otherwise expand into millions of structs + slice growth, tens of MiB per concurrent
	// create. lookupCampaignByName decodes element-by-element and keeps only what it needs, while
	// PRESERVING the omitted-vs-empty distinction (an omitted/null Campaigns field is UNCONFIRMED,
	// a present-but-empty list is a genuine "absent").
	id, matched, present, err := lookupCampaignByName(body, name)
	if err != nil {
		return "", fmt.Errorf("decode QueryByAccountId response: %w", err)
	}
	if !present {
		// The body OMITS or nulls the Campaigns field — unreadable, so we can't confirm the
		// campaign is absent. UNCONFIRMED (verify before create) rather than "absent" (which
		// would let the paid create run and risk a duplicate).
		return "", fmt.Errorf("QueryByAccountId response omitted the Campaigns field; cannot confirm %q is absent", name)
	}
	if matched && id == "" {
		// The name matched but the id is null/unparseable: the campaign almost certainly exists
		// (its unique name matched). Reporting "" (absent) would let CreateCampaigns run and
		// create a DUPLICATE. Error so the caller treats it as UNCONFIRMED (verify before retry).
		return "", fmt.Errorf("campaign %q found in lookup with no usable id", name)
	}
	// id != "" → the match with a usable id; "" with present-empty (no match) → safe to create.
	return id, nil
}

// lookupCampaignByName streams the QueryByAccountId response body and returns, WITHOUT
// materializing the whole Campaigns array, the first case-insensitive name match: its usable
// id (or "" when the matched campaign has no usable id), whether a name match was found, and
// whether the Campaigns field was PRESENT (a non-null array) at all. A malformed 8-MiB body
// therefore costs O(1) memory instead of O(campaigns). present=false means the field was
// omitted or null (→ UNCONFIRMED); present=true with matched=false means a genuine "absent".
//
// It is a thin alias for lookupNamedEntity over the "Campaigns" field: the streaming-parser
// logic (decode → validate array → finishObject → EOF guard) lives in ONE place so a
// correctness fix (truncation handling, null-vs-omit) applies to both the campaign and
// ad-group lookups at once. See lookupNamedEntity in adgroup_ad.go.
func lookupCampaignByName(body []byte, name string) (id string, matched, present bool, err error) {
	return lookupNamedEntity(body, "Campaigns", name)
}

// skipJSONValue consumes exactly one JSON value (object/array/scalar) from dec, discarding it.
// It reads matching open/close delimiters so nested structures are fully skipped without being
// materialized — used to walk past sibling keys cheaply.
func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// errPartialFailure marks a create-Campaigns 200 whose id slot was null AND a
// PartialError was present — a DEFINITE per-entity rejection (the campaign was not
// created), as opposed to a malformed 200 (no id, no error) which is UNCONFIRMED.
var errPartialFailure = errors.New("campaign create reported a PartialError")

// errDuplicateName is a specialization of errPartialFailure: the create was rejected
// because a campaign with this (case-insensitively unique) name already exists. It
// wraps errPartialFailure so errors.Is(err, errPartialFailure) still holds, while
// isDuplicateCampaignNameErr distinguishes the reconcilable already-exists case.
var errDuplicateName = fmt.Errorf("%w (duplicate campaign name)", errPartialFailure)

// errCodeDuplicateCampaign is Microsoft's PartialError code when a campaign name already
// exists. v13 surfaces this either as the string ErrorCode enum
// ("CampaignServiceCannotCreateDuplicateCampaign") OR, in a BatchError, as the numeric Code
// 1115 — the two are the same error, so both must be recognized (see
// isDuplicateCampaignPartial). Matched case-insensitively via codeString semantics.
const (
	errCodeDuplicateCampaign        = "CampaignServiceCannotCreateDuplicateCampaign"
	errCodeDuplicateCampaignNumeric = "1115"
)

// isDuplicateCampaignPartial reports whether a PartialErrors array carries the
// duplicate-campaign-name rejection under EITHER the symbolic ErrorCode enum or the
// equivalent numeric Code 1115.
func isDuplicateCampaignPartial(items []msErrorItem) bool {
	return partialErrorsHaveCode(items, errCodeDuplicateCampaign) ||
		partialErrorsHaveCode(items, errCodeDuplicateCampaignNumeric)
}

// isDuplicateCampaignNameErr reports whether err is the duplicate-name rejection.
func isDuplicateCampaignNameErr(err error) bool { return errors.Is(err, errDuplicateName) }

// errDuplicateKeywords marks an AddKeywords batch in which at least one entry was refused
// because that keyword ALREADY EXISTS on the ad group. It wraps errPartialFailure so any
// existing errors.Is(err, errPartialFailure) classification still matches, but it is
// distinguishable so the ad-group cascade can treat "already attached" as the success it is
// rather than as a rejection.
//
// This is what makes re-posting to a REUSED ad group safe. This client calls no keyword read,
// so a re-run cannot enumerate what is already attached and posts the whole batch; Microsoft
// refuses each already-present keyword instead of creating a second copy, so no criterion is
// duplicated and no bid is doubled. (v13 DOES offer the read — Keywords/QueryByAdGroupId — so
// this is a gap in this client, not an API limit; see createKeywords for the citation.)
var errDuplicateKeywords = fmt.Errorf("%w (keyword already exists on the ad group)", errPartialFailure)

// Microsoft's PartialError codes for a keyword that already exists on the ad group. As with
// the duplicate-campaign codes, v13 surfaces these either as the symbolic ErrorCode enum or
// as the equivalent numeric Code in a BatchError, so both spellings must be recognized.
//
//   - 1517 CampaignServiceDuplicateKeyword: "An attempt was made to create a duplicate of a
//     keyword that already exists."
//   - 1542 CampaignServiceKeywordAndMatchTypeCombinationAlreadyExists: "A keyword with the
//     specified match type already exists."
//
// Both are matched because the same re-post can trip either one: 1517 on the normalized
// keyword text, 1542 when the text is new to the group only in combination with its match
// type.
const (
	errCodeDuplicateKeyword              = "CampaignServiceDuplicateKeyword"
	errCodeDuplicateKeywordNumeric       = "1517"
	errCodeKeywordMatchTypeExists        = "CampaignServiceKeywordAndMatchTypeCombinationAlreadyExists"
	errCodeKeywordMatchTypeExistsNumeric = "1542"
)

// isDuplicateKeywordPartial reports whether EVERY actual error in a PartialErrors array is an
// already-exists keyword rejection (under any of its four spellings).
//
// ALL, not ANY, and the difference is a real defect rather than a stylistic one. A batch can
// mix a duplicate with a GENUINE rejection — an editorial disapproval, a bad bid, an
// over-length term. An ANY test would classify that whole batch as "already attached", return
// nil error, and tell the operator the editorially-rejected keyword "already existed on the ad
// group" — a keyword that in fact does not exist and never will. The run would report success
// for targeting it did not achieve.
//
// So a mixed batch stays on the errPartialFailure path, where the created ids are still
// carried out and the rejection is still surfaced. Only a wholly-duplicate batch is the no-op
// that can be reported as success.
//
// Null placeholder entries are ignored the same way partialErrorsHaveAny ignores them: an
// index-aligned PartialErrors array can carry zero-value items for the entries that succeeded,
// and those are not errors to classify.
func isDuplicateKeywordPartial(items []msErrorItem) bool {
	found := false
	for _, it := range items {
		if codeString(it.ErrorCode) == "" && codeString(it.Code) == "" {
			continue // a null/placeholder slot, not an error
		}
		one := []msErrorItem{it}
		if !partialErrorsHaveCode(one, errCodeDuplicateKeyword) &&
			!partialErrorsHaveCode(one, errCodeDuplicateKeywordNumeric) &&
			!partialErrorsHaveCode(one, errCodeKeywordMatchTypeExists) &&
			!partialErrorsHaveCode(one, errCodeKeywordMatchTypeExistsNumeric) {
			return false
		}
		found = true
	}
	return found
}

// isDuplicateKeywordErr reports whether err is the already-exists keyword rejection.
func isDuplicateKeywordErr(err error) bool { return errors.Is(err, errDuplicateKeywords) }

// firstCampaignID decodes a create-Campaigns 200 body and returns the created
// campaign id. It errors when:
//   - the body is malformed,
//   - CampaignIds[0] is null AND an ACTUAL PartialError is present (errPartialFailure —
//     a definite rejection; errDuplicateName when that PartialError is a duplicate-name),
//   - CampaignIds is empty/null with no actual PartialError (malformed success —
//     UNCONFIRMED).
//
// Per the v13 AddCampaigns contract, PartialErrors is a SPARSE list of BatchError objects —
// it holds a BatchError only for a FAILED item (each carrying an Index into the request), and
// omits successes rather than null-padding them. This client sends a SINGLE campaign per call,
// so a real failure yields exactly one BatchError and a success yields an empty PartialErrors.
// The gate is therefore partialErrorsHaveAny (at least one item carrying an actual code), NOT
// slice length — this also defensively tolerates a malformed body that DID null-pad
// (e.g. {"CampaignIds":[null],"PartialErrors":[null]}): a null-only item carries no code, so it
// stays UNCONFIRMED rather than being mis-reported as a definite rejection.
//
// The caller distinguishes the definite-rejection case via errors.Is(err,
// errPartialFailure) and the already-exists case via isDuplicateCampaignNameErr.
func firstCampaignID(body []byte) (string, error) {
	// Delegate id extraction + the shared three-case classification (valid id → success;
	// null id + actual PartialError → errPartialFailure; else → errNoID) to firstEntityID,
	// so the create-response contract lives in ONE place (see firstEntityID in adgroup_ad.go).
	// Campaigns add only ONE specialization on top: a duplicate-name PartialError is surfaced
	// as errDuplicateName (a wrapper of errPartialFailure) so the caller can self-heal the
	// already-exists race. firstEntityID has no notion of that, so re-inspect the partials
	// here and, when firstEntityID reported a generic errPartialFailure whose cause is the
	// duplicate-name code, upgrade the error to errDuplicateName.
	var partials []msErrorItem
	id, err := firstEntityID(body, "CampaignIds", func(b []byte) ([]*json.Number, []msErrorItem, error) {
		var resp createCampaignsResponse
		if uerr := json.Unmarshal(b, &resp); uerr != nil {
			return nil, nil, uerr
		}
		partials = resp.PartialErrors.Items
		return resp.CampaignIds, resp.PartialErrors.Items, nil
	})
	if err != nil && errors.Is(err, errPartialFailure) && isDuplicateCampaignPartial(partials) {
		return "", fmt.Errorf("%w: %s", errDuplicateName, partialErrorCodes(partials))
	}
	return id, err
}

// partialErrorsHaveAny reports whether the slice contains at least one ACTUAL error —
// an item carrying a non-empty Code or ErrorCode. It filters out the `null` placeholders
// that a position-aligned PartialErrors array can contain (which unmarshal to zero-value
// items), so a slice of only-null entries is treated as "no error".
func partialErrorsHaveAny(items []msErrorItem) bool {
	for _, it := range items {
		if codeString(it.ErrorCode) != "" || codeString(it.Code) != "" {
			return true
		}
	}
	return false
}

// idRE matches a POSITIVE integer id (no sign, decimal point, or exponent). Microsoft
// entity ids are positive int64s, so a negative/fractional/exponent-form JSON number is
// malformed and must NOT be accepted as a valid id.
var idRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// numberID renders a *json.Number id to a trimmed string, returning "" for a nil id or
// any value that is not a positive integer. A *json.Number preserves the raw JSON token,
// so this rejects "0", "-1", "1.5", and "1e3" — accepting one of those would report a
// malformed 200 as a successful create with an unusable id instead of UNCONFIRMED.
func numberID(n *json.Number) string {
	if n == nil {
		return ""
	}
	id := strings.TrimSpace(n.String())
	if !idRE.MatchString(id) {
		return ""
	}
	// Microsoft resource ids are signed 64-bit. A digits-only value that OVERFLOWS int64
	// can't be a real id, so reject it (→ "" → UNCONFIRMED/no-id) rather than accept a bogus
	// id the regex alone would pass. ParseInt enforces the range in base 10.
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return ""
	}
	return id
}

// partialErrorsHaveCode reports whether any PartialError carries the given code
// (case-insensitive, matching hasErrorCode).
func partialErrorsHaveCode(items []msErrorItem, code string) bool {
	for _, it := range items {
		for _, raw := range []json.RawMessage{it.ErrorCode, it.Code} {
			if strings.EqualFold(codeString(raw), code) {
				return true
			}
		}
	}
	return false
}

// partialErrorCodes renders the machine-readable codes from a PartialErrors array for
// an error message. Only the codes are surfaced (never Message/Details, which can echo
// account/entity specifics), matching the apiError contract. Bounded by the same
// per-code length/count caps used for non-2xx bodies.
func partialErrorCodes(items []msErrorItem) string {
	var codes []string
	for _, it := range items {
		for _, raw := range []json.RawMessage{it.ErrorCode, it.Code} {
			if v := codeString(raw); v != "" && len(v) <= maxErrorCodeLen {
				codes = append(codes, v)
				if len(codes) >= maxRetainedErrorCodes {
					return strings.Join(codes, ",")
				}
			}
		}
	}
	if len(codes) == 0 {
		return "unspecified"
	}
	return strings.Join(codes, ",")
}

// msDate is Microsoft's date object ({Month,Day,Year}), used by ad-group flight dates
// (a later slice). Microsoft does NOT accept an ISO-8601 string for these fields — it
// requires the object form — so a helper is provided now to keep the serialization in
// one reviewed place. Not yet used by any slice — the flight-date work will be its first
// caller, and naming a specific slice here only dates the comment (MS-4 turned out to be the
// keyword/bid work, which does not touch it).
type msDate struct {
	Month int `json:"Month"`
	Day   int `json:"Day"`
	Year  int `json:"Year"`
}

// toMSDate converts a time.Time to Microsoft's {Month,Day,Year} form. The caller is
// responsible for supplying a time already in the account's intended time zone.
func toMSDate(t time.Time) msDate {
	return msDate{Month: int(t.Month()), Day: t.Day(), Year: t.Year()}
}

// composeName builds a deterministic campaign name from the input. The NameSuffix
// (when supplied) makes it unique+stable per logical campaign so a retry composes the
// SAME name and findCampaignByName returns the existing campaign rather than
// double-creating. A stable name is a reliable idempotency key precisely BECAUSE Microsoft
// rejects a duplicate campaign name (code 1115) — the find-first avoids that rejection on a
// retry. Mirrors the google-ads composer.
func composeName(in CampaignInput) string {
	parts := []string{"LFX", "Search Campaign"}
	if p := sanitizeNamePart(in.Project); p != "" {
		parts = append(parts, p)
	}
	if e := sanitizeNamePart(in.EventName); e != "" {
		parts = append(parts, e)
	}
	if s := sanitizeNamePart(in.NameSuffix); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

// sanitizeNamePart trims a caller-supplied name segment and strips the "|" delimiter
// (a raw "|" would inject extra fields into the pipe-delimited name and break project
// attribution / name-based reconciliation). It also replaces ANY control character
// (incl. NUL) with a space and collapses whitespace runs, so an embedded control char
// can't reach a paid create as an invalid name. Mirrors the google-ads sanitizer.
func sanitizeNamePart(s string) string {
	s = strings.ReplaceAll(s, "|", " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// validateEntityName rejects an empty or over-length composed name before any create
// call. Measured in the unit Microsoft enforces for the field (characters for
// Campaign.Name); the caller passes the measured length and unit label so the check
// matches the service's own limit. Mirrors the google-ads validator.
func validateEntityName(kind, name string, measuredLen, maxLen int, unit string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("microsoft-ads %s name is empty", kind)
	}
	if measuredLen > maxLen {
		return fmt.Errorf("microsoft-ads %s name exceeds %d %s (%d): shorten EventName/Project/NameSuffix", kind, maxLen, unit, measuredLen)
	}
	return nil
}

// Run states a Microsoft campaign/ad group/ad can be toggled between. Microsoft's Status
// enums spell the serving state "Active" (the create path uses "Paused" from the same enum).
const (
	// StatusActive lets an entity serve.
	StatusActive = "Active"
	// StatusPaused stops an entity serving.
	StatusPaused = "Paused"
)

// updateCampaignsRequest / updateAdGroupsRequest / updateAdsRequest are the PUT bodies for a
// status-only update. Each carries the entity Id plus Status; every other field is omitted so
// the PUT is a partial update and cannot clobber budget, schedule, or targeting.
type updateCampaignsRequest struct {
	AccountId json.Number        `json:"AccountId"`
	Campaigns []msCampaignStatus `json:"Campaigns"`
}

type msCampaignStatus struct {
	Id     json.Number `json:"Id"`
	Status string      `json:"Status"`
}

type updateAdGroupsRequest struct {
	CampaignId json.Number       `json:"CampaignId"`
	AdGroups   []msAdGroupStatus `json:"AdGroups"`
}

type msAdGroupStatus struct {
	Id     json.Number `json:"Id"`
	Status string      `json:"Status"`
}

type updateAdsRequest struct {
	AdGroupId json.Number  `json:"AdGroupId"`
	Ads       []msAdStatus `json:"Ads"`
}

type msAdStatus struct {
	Id     json.Number `json:"Id"`
	Status string      `json:"Status"`
}

// updateKeywordsRequest is the PUT /Keywords body for a status-only update. Like the ad
// update it is scoped by AdGroupId (a keyword is addressed through its ad group); every
// field other than Id/Status is omitted so the PUT is a partial update and cannot clobber
// a keyword's bid or destination.
type updateKeywordsRequest struct {
	AdGroupId json.Number       `json:"AdGroupId"`
	Keywords  []msKeywordStatus `json:"Keywords"`
}

type msKeywordStatus struct {
	Id     json.Number `json:"Id"`
	Status string      `json:"Status"`
}

// updateStatusResponse is the (subset of the) 200 body every status PUT returns. Microsoft
// reports a per-entity failure as HTTP 200 with a populated PartialErrors — so a naive
// err == nil check would silently report success for a REJECTED status change.
//
// The field's PRESENCE is tracked separately from its value, because absence and emptiness mean
// different things here: `{"PartialErrors": null}` is Microsoft affirming no entity failed, while
// `{}` or a top-level `null` is a body that never spoke to the question. Both leave PartialErrors
// zero, so without sawPartialErrors an unrecognisable success body reports success and the service
// persists a status Microsoft never confirmed.
type updateStatusResponse struct {
	PartialErrors    boundedErrorItems `json:"PartialErrors"`
	sawPartialErrors bool
}

// UnmarshalJSON records whether PartialErrors was present, then decodes it normally.
//
// A top-level `null` is delivered to UnmarshalJSON as the literal "null" and must leave
// sawPartialErrors false: it carries no field at all. Decoding into an alias type avoids
// recursing back into this method.
func (r *updateStatusResponse) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if _, ok := probe["PartialErrors"]; !ok {
		// Leaves sawPartialErrors false; putStatus turns that into an unconfirmed outcome.
		return nil
	}
	type alias updateStatusResponse
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	r.PartialErrors = decoded.PartialErrors
	// `null` and `[]` are Microsoft's valid ways of saying "no per-entity failures", so both
	// count as the field having been ANSWERED.
	r.sawPartialErrors = true
	return nil
}

// IsOutcomeUnconfirmed reports whether err leaves the mutation's outcome AMBIGUOUS —
// Microsoft may have applied it despite the error, so the caller must VERIFY before retrying
// rather than assume "not applied". Exported so the dispatcher can classify across the
// package boundary (mirrors the reddit/twitter/googleads clients' helper of the same name).
func IsOutcomeUnconfirmed(err error) bool {
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		return true
	}
	return createOutcomeAmbiguous(err)
}

// partialCascadeError marks a cascade that PARTIALLY applied: an upstream entity was already
// changed before a downstream one failed. It reports Unconfirmed() so IsOutcomeUnconfirmed
// classifies it as verify-before-retry — a DEFINITE rejection on the child is still an
// ambiguous OVERALL outcome, because the parent change did land.
type partialCascadeError struct {
	// applied is the entity whose status DID change; stage is the one that then failed.
	applied string
	stage   string
	err     error
}

func (e *partialCascadeError) Error() string {
	return "microsoft-ads: " + e.applied + " status changed but the " + e.stage + " update failed (partially applied): " + e.err.Error()
}
func (e *partialCascadeError) Unwrap() error { return e.err }

// Unconfirmed marks the outcome as ambiguous-applied for IsOutcomeUnconfirmed.
func (e *partialCascadeError) Unconfirmed() bool { return true }

// putStatus issues one status-only PUT and folds Microsoft's 200-with-PartialErrors contract
// into an ordinary error, so callers can't mistake a rejected update for a success.
//
// The PUT is passed as IDEMPOTENT, so a 429 is retried under doRequest's bounded backoff. This
// request only SETS a desired status — re-applying Active/Paused converges on the same state, and
// unlike a create it cannot double-commit a paid resource — so the double-write risk that makes
// creates non-retryable does not exist here. Passing false instead turned ordinary Microsoft
// throttling into an Unconfirmed toggle that the dispatcher then had to verify before retrying,
// which is a strictly worse outcome than letting the client absorb the 429. Matches the sibling
// Reddit status setter (internal/platform/reddit/client.go, updateEntityStatus).
func (c *Client) putStatus(ctx context.Context, path string, req any, entity string) error {
	body, err := c.doRequest(ctx, http.MethodPut, path, req, true)
	if err != nil {
		return err
	}
	var resp updateStatusResponse
	if uerr := json.Unmarshal(body, &resp); uerr != nil {
		// A malformed 200 leaves the outcome UNKNOWN: the update MAY have applied, so do not
		// report success. transportError reports Unconfirmed, matching the create path's
		// treatment of an undecodable success body.
		return &transportError{Method: http.MethodPut, Path: path, err: fmt.Errorf("decode %s status response: %w", entity, uerr)}
	}
	// A syntactically valid body that OMITS PartialErrors (`{}`, or a top-level `null`) decodes
	// without error and leaves the field zero, which partialErrorsHaveAny reads as "no rejection".
	// That would report success for a status Microsoft never confirmed. The field's ABSENCE is
	// therefore treated as an unconfirmed outcome; its valid empty forms (`null`, `[]`) are still
	// accepted, since those are how Microsoft says "no per-entity failures".
	if !resp.sawPartialErrors {
		return &transportError{Method: http.MethodPut, Path: path, err: fmt.Errorf("decode %s status response: response omitted PartialErrors", entity)}
	}
	// A present but malformed PartialErrors array such as `[null]` or `[{}]` decodes without
	// error but contains no valid error codes — partialErrorsHaveAny returns false, which would
	// report success for a status Microsoft never confirmed. Reject any non-empty list that
	// yields no valid codes (mirroring the create path's handling of null-only error responses).
	if len(resp.PartialErrors.Items) > 0 && !partialErrorsHaveAny(resp.PartialErrors.Items) {
		return &transportError{Method: http.MethodPut, Path: path, err: fmt.Errorf("decode %s status response: PartialErrors present but contains no valid error codes", entity)}
	}
	if partialErrorsHaveAny(resp.PartialErrors.Items) {
		return fmt.Errorf("microsoft-ads rejected the %s status update: %s", entity, partialErrorCodes(resp.PartialErrors.Items))
	}
	return nil
}

// UpdateCampaignAndChildrenStatus toggles a Microsoft campaign and its ad group, ad and
// keywords between Active and Paused.
//
// FULL CASCADE, like reddit: CreateCampaign builds the whole Campaign -> AdGroup -> Ad tree
// PAUSED — and MS-4 creates the KEYWORDS Paused too — so toggling only the campaign would
// leave the descendants paused and nothing serving.
//
// ORDER follows the same INVARIANT as reddit — the campaign is the gate, so it flips LAST on
// ACTIVATE (nothing serves until the tree is ready) and FIRST on PAUSE (delivery stops
// immediately even if a later call fails). The non-gate entities are order-independent
// between themselves: while the gate is still Paused nothing serves whichever goes first, so
// this activates ad group, then ad, then keywords (reddit happens to go deepest-first; that
// difference is not load-bearing).
//
// KEYWORDS are part of the cascade because MS-4 creates them Paused deliberately (so an
// operator reviews the list before it spends). That choice is only coherent if the
// service-driven enable turns them on: otherwise activation would enable the campaign, ad
// group and ad while every keyword stayed Paused, and the campaign would serve nothing while
// reporting Active — the precise lie this cascade exists to prevent. An EMPTY keywordIDs
// slice skips the keyword call entirely rather than sending an empty PUT.
//
// Activating with an unknown ad-group OR ad id is refused: the missing child would stay
// Paused and nothing would serve, so reporting "active" would be a lie. Pausing needs no
// child ids — pausing the parent already stops delivery — EXCEPT that an ad id with no
// ad-group id is refused outright, because the Ads PUT is scoped by AdGroupId and so cannot
// address the ad at all. Keyword ids are likewise addressed through the ad group, so they are
// skipped when it is unknown.
//
// Once an entity has been changed, a later failure returns a partialCascadeError
// (Unconfirmed) so a definite rejection on a child is not misreported as "not modified".
func (c *Client) UpdateCampaignAndChildrenStatus(ctx context.Context, campaignID, adGroupID, adID string, keywordIDs []string, status string) error {
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("microsoft-ads: unsupported status %q (want %s or %s)", status, StatusActive, StatusPaused)
	}
	if !idRE.MatchString(strings.TrimSpace(campaignID)) {
		return fmt.Errorf("microsoft-ads: campaign id %q is not a numeric id", campaignID)
	}
	cID := strings.TrimSpace(campaignID)
	agID, aID := strings.TrimSpace(adGroupID), strings.TrimSpace(adID)
	if status == StatusActive && (agID == "" || aID == "") {
		return fmt.Errorf("microsoft-ads: cannot activate campaign %s: its ad group and ad ids must both be known, so the tree cannot be made servable", campaignID)
	}
	// An ad is addressed BY ITS PARENT (the Ads PUT is scoped by AdGroupId), so an ad id
	// without an ad-group id cannot be actioned at all. Reject the pair rather than skipping
	// the ad: silently skipping would leave the ad Active while this call returned nil, and
	// sending it anyway would marshal the empty parent as a bare 0 (json.Number("") encodes
	// as 0, not ""), addressing a nonexistent ad group and reporting a no-op as success.
	if aID != "" && agID == "" {
		return fmt.Errorf("microsoft-ads: campaign %s has ad %s but no ad group id: the ad is addressed by its ad group, so its status cannot be changed", campaignID, aID)
	}
	for _, id := range []struct{ label, val string }{{"ad group", agID}, {"ad", aID}} {
		if id.val != "" && !idRE.MatchString(id.val) {
			return fmt.Errorf("microsoft-ads: %s id %q is not a numeric id", id.label, id.val)
		}
	}
	// Validate EVERY keyword id before any mutation, not lazily while building the request: a
	// bad id discovered mid-cascade would fail after the campaign/ad group/ad were already
	// flipped, turning a rejectable input error into an unconfirmed partial cascade. A
	// keyword is addressed through its ad group, so ids are unusable without one — the same
	// rule the ad follows.
	kwIDs := make([]string, 0, len(keywordIDs))
	for _, k := range keywordIDs {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !idRE.MatchString(k) {
			return fmt.Errorf("microsoft-ads: keyword id %q is not a numeric id", k)
		}
		kwIDs = append(kwIDs, k)
	}
	if len(kwIDs) > 0 && agID == "" {
		return fmt.Errorf("microsoft-ads: campaign %s has %d keyword id(s) but no ad group id: keywords are addressed by their ad group, so their status cannot be changed", campaignID, len(kwIDs))
	}

	campaignReq := updateCampaignsRequest{
		AccountId: json.Number(c.account.AccountID),
		Campaigns: []msCampaignStatus{{Id: json.Number(cID), Status: status}},
	}
	adGroupReq := updateAdGroupsRequest{
		CampaignId: json.Number(cID),
		AdGroups:   []msAdGroupStatus{{Id: json.Number(agID), Status: status}},
	}
	adReq := updateAdsRequest{
		AdGroupId: json.Number(agID),
		Ads:       []msAdStatus{{Id: json.Number(aID), Status: status}},
	}
	keywordStatuses := make([]msKeywordStatus, 0, len(kwIDs))
	for _, k := range kwIDs {
		keywordStatuses = append(keywordStatuses, msKeywordStatus{Id: json.Number(k), Status: status})
	}
	keywordReq := updateKeywordsRequest{
		AdGroupId: json.Number(agID),
		Keywords:  keywordStatuses,
	}

	if status == StatusActive {
		// DESCENDANTS FIRST, campaign gate LAST. Both child ids are guaranteed present above.
		if err := c.putStatus(ctx, "AdGroups", adGroupReq, "ad group"); err != nil {
			return err // nothing mutated yet — a definite rejection stays definite
		}
		if err := ctx.Err(); err != nil {
			return &partialCascadeError{applied: "ad group", stage: "ad", err: err}
		}
		if err := c.putStatus(ctx, "Ads", adReq, "ad"); err != nil {
			return &partialCascadeError{applied: "ad group", stage: "ad", err: err}
		}
		applied := "ad group and ad"
		// Keywords BEFORE the campaign gate, for the same reason as the other descendants:
		// nothing serves while the gate is Paused, so enabling them first is free, whereas
		// enabling them after the gate would open a window in which the campaign is Active with
		// every keyword still Paused.
		if len(keywordStatuses) > 0 {
			if err := ctx.Err(); err != nil {
				return &partialCascadeError{applied: applied, stage: "keywords", err: err}
			}
			if err := c.putStatus(ctx, "Keywords", keywordReq, "keywords"); err != nil {
				return &partialCascadeError{applied: applied, stage: "keywords", err: err}
			}
			applied = "ad group, ad and keywords"
		}
		if err := ctx.Err(); err != nil {
			return &partialCascadeError{applied: applied, stage: "campaign", err: err}
		}
		if err := c.putStatus(ctx, "Campaigns", campaignReq, "campaign"); err != nil {
			return &partialCascadeError{applied: applied, stage: "campaign", err: err}
		}
		return nil
	}

	// PAUSE: campaign gate first — delivery stops now, even if a child call fails below.
	if err := c.putStatus(ctx, "Campaigns", campaignReq, "campaign"); err != nil {
		return err // nothing mutated yet
	}
	// applied tracks what ACTUALLY changed, so a later failure names only entities that were
	// really touched — this text is what an operator reads to decide what to verify by hand.
	applied := "campaign"
	if agID != "" {
		if err := ctx.Err(); err != nil {
			return &partialCascadeError{applied: applied, stage: "ad group", err: err}
		}
		if err := c.putStatus(ctx, "AdGroups", adGroupReq, "ad group"); err != nil {
			return &partialCascadeError{applied: applied, stage: "ad group", err: err}
		}
		applied = "campaign and ad group"
	}
	if aID != "" {
		if err := ctx.Err(); err != nil {
			return &partialCascadeError{applied: applied, stage: "ad", err: err}
		}
		if err := c.putStatus(ctx, "Ads", adReq, "ad"); err != nil {
			return &partialCascadeError{applied: applied, stage: "ad", err: err}
		}
		applied += " and ad"
	}
	// Keywords LAST on pause. The campaign gate above already stopped delivery, so this is
	// housekeeping that leaves the tree in the same all-Paused shape CreateCampaign produces
	// — which is what makes a later re-activate symmetric with a fresh create.
	if len(keywordStatuses) > 0 {
		if err := ctx.Err(); err != nil {
			return &partialCascadeError{applied: applied, stage: "keywords", err: err}
		}
		if err := c.putStatus(ctx, "Keywords", keywordReq, "keywords"); err != nil {
			return &partialCascadeError{applied: applied, stage: "keywords", err: err}
		}
	}
	return nil
}
