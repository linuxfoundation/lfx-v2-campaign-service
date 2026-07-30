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
	"net"
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
}

// CampaignResult reports what CreateCampaign created (or found). The campaign NAME
// matters on an ambiguous failure BEFORE an id is known: the name is deterministic,
// so a caller reconciling a possibly-created campaign looks it up by CampaignName.
type CampaignResult struct {
	Platform     string `json:"platform"`
	AccountLabel string `json:"accountLabel,omitempty"`
	CampaignName string `json:"campaignName"`
	CampaignID   string `json:"campaignId"`
	// AdGroupName / AdGroupID identify the ad group created (or found) under the
	// campaign. AdGroupName is deterministic, so an ambiguous ad-group failure BEFORE an
	// id is known is reconcilable by name (scoped to the campaign).
	AdGroupName string `json:"adGroupName,omitempty"`
	AdGroupID   string `json:"adGroupId,omitempty"`
	// AdID identifies the Responsive Search Ad created under the ad group.
	AdID string `json:"adId,omitempty"`
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
		uni, ierr := idna.Lookup.ToUnicode(host)
		if ierr != nil {
			return nil, fmt.Errorf("microsoft-ads composed ad final URL host %q is not a valid IDNA label: %w", host, ierr)
		}
		if uni != "" {
			host = uni
		}
		authority := host
		// Lower-case the scheme before the default-port test: validateAdURL accepts any scheme
		// casing and buildAdFinalURL preserves it, so a valid HTTPS://…:443 would otherwise
		// miss the case-sensitive "https" match and wrongly count :443 against the host length.
		if port := u.Port(); port != "" && !isDefaultPort(strings.ToLower(u.Scheme), port) {
			authority = net.JoinHostPort(host, port)
		}
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

	var steps []string
	microsoftAdsURL := "https://ads.microsoft.com/campaign/vnext/campaigns?aid=" + c.account.AccountID

	namePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:        "microsoft-ads",
			AccountLabel:    c.account.Label,
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
	campaignPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignID = campaignID
		return r
	}

	// Steps 3-4: complete the Campaign -> AdGroup -> Ad hierarchy (all PAUSED) so the
	// result is a usable paused campaign rather than an empty shell.
	return c.createAdGroupAndAd(ctx, in, campaignID, alreadyExisted, &steps, campaignPartial)
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
		partials = resp.PartialErrors
		return resp.CampaignIds, resp.PartialErrors, nil
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
// one reviewed place. Reserved for MS-3.
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
