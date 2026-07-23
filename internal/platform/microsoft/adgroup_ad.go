// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Ad group + ad creation (MS-2.5): complete the Campaign -> AdGroup -> Ad tree
//
// CreateCampaign creates the campaign (MS-2), then this file finishes the
// hierarchy so the result is a usable PAUSED campaign rather than an empty shell —
// mirroring the reddit/meta clients, whose CreateCampaign creates all three levels.
// Everything stays PAUSED so nothing serves until a human enables it.
//
// The same two Microsoft transport facts from campaign.go apply at every level:
// PartialErrors-on-200 (a per-entity failure is a 200 with a null id slot + a
// PartialError, inspected via firstEntityID) and duplicate-names-allowed (so each
// level is find-or-create by its deterministic name before a create).
//
// Ad type: v13 does NOT support ADDING a TextAd/ExpandedTextAd (every TextAd field is
// "Add: Not supported"; a standard text ad add fails with CampaignServiceAdTypeInvalid).
// The currently-addable Search text ad is the ResponsiveSearchAd — 3-15 headline assets
// and 2-4 description assets (each a TextAsset in an AssetLink) plus a required FinalUrls.
// Its ad group must be AdGroupType "SearchStandard" to accept it.
// ---------------------------------------------------------------------------

const (
	// adGroupStatusPaused creates the ad group PAUSED (Microsoft's AdGroup.Status enum
	// value). The RESPONSIVE SEARCH AD created under it also carries Status "Paused" —
	// Ad.Status defaults to Active on Add, so the ad must set Paused explicitly or it
	// would be eligible to serve the moment a human enables the campaign/ad group.
	adGroupStatusPaused = "Paused"
	adStatusPaused      = "Paused"

	// adGroupTypeSearchStandard is the AdGroup.AdGroupType required to host a Responsive
	// Search Ad. In a Search campaign a "SearchDynamic" ad group takes only dynamic search
	// ads; "SearchStandard" (the Search default) is the one that accepts responsive search
	// ads. Sent explicitly so the ad-group/ad-type pairing can't drift on a default change.
	adGroupTypeSearchStandard = "SearchStandard"

	// adGroupLanguage is the AdGroup.Language sent on create. Language is "Optional if the
	// campaign has one or more languages set, and otherwise required for most campaign
	// types". The MS-2 campaign create sets no campaign-level Languages, so the ad group
	// must carry one or AddAdGroups is rejected. English is the safe default for a PAUSED
	// broker shell; a human can retarget before enabling.
	adGroupLanguage = "English"

	// maxAdGroupNameRunes bounds the composed ad-group name. Microsoft limits
	// AdGroup.Name to 256 characters; validated in runes before the create.
	maxAdGroupNameRunes = 256

	// Responsive Search Ad asset limits (final characters, per the v13 ResponsiveSearchAd
	// contract). SINGLE-width copy: headline <=30, description <=90. DOUBLE-width copy (any
	// CJK / Korean / Japanese / Chinese character or emoji) uses Microsoft's reduced limits:
	// headline <=15, description <=45. adHeadlineLimit/adDescriptionLimit pick the right cap
	// per string; a value is bounded to it before the create so an over-limit asset is
	// rejected up front (the ad is PAUSED, so truncating a placeholder is acceptable).
	maxAdHeadlineRunes        = 30
	maxAdDescriptionRunes     = 90
	maxAdHeadlineRunesWide    = 15
	maxAdDescriptionRunesWide = 45

	// maxFinalURLRunes bounds the ad's composed FinalUrls (the registration URL with the LFX
	// utm_* params appended). Microsoft limits a Final URL to 2,048 characters including the
	// protocol; validated on the COMPOSED url up front so a near-limit registration URL can't
	// pass and then be rejected at AddAds after the campaign/ad group already exist.
	maxFinalURLRunes = 2048

	// Responsive Search Ad asset-count bounds (v13 "Add: Required"): 3-15 UNIQUE headlines
	// and 2-4 UNIQUE descriptions. The composer emits counts inside these ranges; a shortfall
	// or over-count is a clean up-front validation error, not a rejected paid create.
	minAdHeadlines    = 3
	maxAdHeadlines    = 15
	minAdDescriptions = 2
	maxAdDescriptions = 4

	// adTypeResponsiveSearch is the ad type this client creates. It is sent BOTH as the
	// polymorphic "Type" discriminator in the AddAds body (so the service deserializes the
	// entry as a ResponsiveSearchAd — "Add:Read-only" bars CHANGING the type, not the wire
	// discriminator) AND as the required AdTypes filter on the Ads/QueryByAdGroupId lookup.
	adTypeResponsiveSearch = "ResponsiveSearch"
)

// msAdGroup is one AdGroup in the POST /AdGroups body. Only Name is strictly Add:Required,
// but AdGroupType and Language are set explicitly (Language is conditionally required when
// the campaign sets no languages; AdGroupType pins the group to the responsive-search-ad-
// capable "SearchStandard"). Status is set PAUSED (its Add default is already Paused, sent
// explicitly for clarity). Targeting/bids use account defaults — the conservative choice
// for a broker-created shell.
type msAdGroup struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	AdGroupType string `json:"AdGroupType"`
	Language    string `json:"Language"`
}

// createAdGroupsRequest is the POST /AdGroups body. The v13 AddAdGroups operation
// REQUIRES CampaignId at the top level (a sibling to AdGroups) — the target campaign is
// NOT in the URL. ReturnInheritedBidStrategyTypes is also a body element the docs list as
// required ("unless otherwise noted... all request elements are required"; it's marked
// "reserved for future use" but carries no optional note), so it is sent as false. Response
// is an index-aligned id slice + PartialErrors (a null slot = that entity failed).
type createAdGroupsRequest struct {
	CampaignId                      json.Number `json:"CampaignId"`
	AdGroups                        []msAdGroup `json:"AdGroups"`
	ReturnInheritedBidStrategyTypes bool        `json:"ReturnInheritedBidStrategyTypes"`
}

type createAdGroupsResponse struct {
	AdGroupIds    []*json.Number `json:"AdGroupIds"`
	PartialErrors []msErrorItem  `json:"PartialErrors"`
}

// queryAdGroupsRequest is the POST /AdGroups/QueryByCampaignId body used by
// findAdGroupByName — the v13 GetAdGroupsByCampaignId REST operation is a POST with the
// CampaignId in the body, not a GET.
type queryAdGroupsRequest struct {
	CampaignId json.Number `json:"CampaignId"`
}

// queryAdGroupsResponse is the (subset of the) QueryByCampaignId response.
type queryAdGroupsResponse struct {
	AdGroups []struct {
		Id   *json.Number `json:"Id"`
		Name string       `json:"Name"`
	} `json:"AdGroups"`
}

// msTextAsset is a TextAsset carried inside an AssetLink. Microsoft stores a responsive
// search ad's headlines/descriptions as text assets (one TextAsset per AssetLink); the
// Type discriminator "TextAsset" is required so the polymorphic Asset deserializes.
type msTextAsset struct {
	Type string `json:"Type"` // always "TextAsset"
	Text string `json:"Text"`
}

// msAssetLink wraps one asset in the Headlines/Descriptions lists. Only the nested Asset
// is set on create; PinnedField/EditorialStatus/AssetPerformanceLabel are omitted (Bing
// optimizes layout freely for an unpinned asset).
type msAssetLink struct {
	Asset msTextAsset `json:"Asset"`
}

// msResponsiveSearchAd is one ResponsiveSearchAd in the POST /Ads body. v13 does NOT
// support adding TextAd/ExpandedTextAd (Add: Not supported → CampaignServiceAdTypeInvalid);
// the responsive search ad is the currently-addable Search text ad. Add:Required fields
// are Headlines (3-15), Descriptions (2-4), and FinalUrls; Status is set PAUSED (its Add
// default is Active).
//
// Type IS sent: the AddAds body is POLYMORPHIC (an array of the base Ad), and the REST JSON
// uses a "Type" property as the DISCRIMINATOR that selects the derived subtype to
// deserialize into (the AddAds REST example shows e.g. "Type":"AppInstall"). "Add:Read-only"
// on Ad.Type means the value can't be CHANGED, not that the wire discriminator is omitted —
// without it the service can't tell this is a ResponsiveSearchAd and rejects the create.
type msResponsiveSearchAd struct {
	Type         string        `json:"Type"`
	Headlines    []msAssetLink `json:"Headlines"`
	Descriptions []msAssetLink `json:"Descriptions"`
	FinalUrls    []string      `json:"FinalUrls"`
	Status       string        `json:"Status"`
}

// createAdsRequest is the POST /Ads body. The v13 AddAds operation REQUIRES AdGroupId
// at the top level (a sibling to Ads) — the target ad group is NOT in the URL.
type createAdsRequest struct {
	AdGroupId json.Number            `json:"AdGroupId"`
	Ads       []msResponsiveSearchAd `json:"Ads"`
}

type createAdsResponse struct {
	AdIds         []*json.Number `json:"AdIds"`
	PartialErrors []msErrorItem  `json:"PartialErrors"`
}

// queryAdsRequest is the POST /Ads/QueryByAdGroupId body used by findAdByFinalURL. Unlike
// AdGroups/QueryByCampaignId (only CampaignId), GetAdsByAdGroupId marks AdTypes REQUIRED
// ("unless otherwise noted... all request elements are required", and only
// ReturnAdditionalFields is noted optional) — omitting it rejects the lookup before the ad
// create is reached. We query the ResponsiveSearch type this client creates.
type queryAdsRequest struct {
	AdGroupId json.Number `json:"AdGroupId"`
	AdTypes   []string    `json:"AdTypes"`
}

// createAdGroupAndAd completes the hierarchy under an already-created/found campaign:
// it find-or-creates a PAUSED ad group, then creates a PAUSED Text Ad under it. Each
// step accumulates its ids into the result so an ambiguous failure at a later step
// leaves the whole tree reconcilable, never orphaned anonymously.
//
// campaignPartial() returns the result carrying everything known so far (campaign id +
// name); this function extends it with the ad-group and ad ids/names as they land.
func (c *Client) createAdGroupAndAd(
	ctx context.Context,
	in CampaignInput,
	campaignID string,
	alreadyExisted bool,
	steps *[]string,
	campaignPartial func() *CampaignResult,
) (*CampaignResult, error) {
	// The ad destination URL (in.RegistrationURL) is validated up front in CreateCampaign,
	// BEFORE the campaign create, so a bad URL fails cleanly without orphaning a PAUSED
	// campaign or ad group. No re-validation here: the input hasn't changed, and repeating
	// it would only risk the two checks drifting apart.

	adGroupName := composeAdGroupName(in)
	if err := validateEntityName("ad group", adGroupName, utf8.RuneCountInString(adGroupName), maxAdGroupNameRunes, "characters"); err != nil {
		return campaignPartial(), fmt.Errorf("microsoft-ads ad group name invalid (campaign %s created): %w", campaignID, err)
	}

	// adGroupPartial carries the campaign id/name + the ad-group name (and, once known,
	// its id) so an ambiguous ad-group/ad failure is reconcilable.
	adGroupPartial := func() *CampaignResult {
		r := campaignPartial()
		r.AdGroupName = adGroupName
		return r
	}

	// Step 3: find-or-create the ad group under the campaign. The lookup is a read
	// (idempotent), the create is a mutation (not retried on 429). A cancellation
	// during the lookup is a clean abort (nothing new created), but the CAMPAIGN
	// already exists, so it is surfaced as a reconcilable partial rather than (nil,err).
	adGroupID, existed, err := c.findOrCreateAdGroup(ctx, campaignID, adGroupName)
	if err != nil {
		// ORDER MATTERS. createOutcomeAmbiguous catches a transportError FIRST — a ctx-cancel
		// mid-HTTP-Do is wrapped as a transportError (whose Unwrap exposes context.Canceled),
		// and that create MAY have committed, so it must stay UNCONFIRMED. Only a BARE context
		// error (from the read lookup's backoff/pre-send, not wrapped in transportError) is a
		// clean abort where nothing was created. errNoID (malformed 2xx, no id) is UNCONFIRMED.
		switch {
		case createOutcomeAmbiguous(err) || errors.Is(err, errNoID):
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation UNCONFIRMED (campaign %s created; %q may exist — verify before retrying): %w", campaignID, adGroupName, err)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group step aborted (campaign %s created; context done during the lookup, no ad group created): %w", campaignID, err)
		case errors.Is(err, errPartialFailure):
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation rejected (campaign %s created): %w", campaignID, err)
		default:
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation failed (campaign %s created): %w", campaignID, err)
		}
	}
	adGroupExisted := existed
	if existed {
		*steps = append(*steps, fmt.Sprintf("Ad group already exists by name: %s (not re-created)", adGroupID))
	} else {
		*steps = append(*steps, fmt.Sprintf("Ad group created: %s (PAUSED)", adGroupID))
	}

	adGroupWithIDPartial := func() *CampaignResult {
		r := adGroupPartial()
		r.AdGroupID = adGroupID
		return r
	}

	// If the context is ALREADY done after the ad-group step, abort cleanly BEFORE firing
	// any ad lookup/create HTTP work — the campaign + ad group ids are known and returned in
	// a reconcilable partial, and nothing new is attempted.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad step aborted (campaign %s + ad group %s created; context done before the ad step, no ad created): %w", campaignID, adGroupID, ctxErr)
	}

	// Step 4: create the PAUSED Responsive Search Ad under the ad group. v13 does not
	// support adding text/expanded-text ads, so the ad is a responsive search ad (3-15
	// headline assets + 2-4 description assets). Ads carry no stable human name, so
	// idempotency is by destination: look for an existing ad whose FinalUrls already
	// contains this URL before creating, so a retry doesn't stack a duplicate ad.
	headlines, descriptions := composeAdCopy(in)
	finalURL := buildAdFinalURL(in)

	adID, existed, err := c.findOrCreateResponsiveSearchAd(ctx, adGroupID, headlines, descriptions, finalURL)
	if err != nil {
		// Same ordered classification as the ad group (ambiguous transport/errNoID first, so a
		// mid-flight ctx-cancel stays UNCONFIRMED; a bare context error from the lookup is a
		// clean abort).
		switch {
		case createOutcomeAmbiguous(err) || errors.Is(err, errNoID):
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation UNCONFIRMED (campaign %s + ad group %s created; an ad may exist — verify before retrying): %w", campaignID, adGroupID, err)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad step aborted (campaign %s + ad group %s created; context done during the lookup, no ad created): %w", campaignID, adGroupID, err)
		case errors.Is(err, errPartialFailure):
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation rejected (campaign %s + ad group %s created): %w", campaignID, adGroupID, err)
		default:
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation failed (campaign %s + ad group %s created): %w", campaignID, adGroupID, err)
		}
	}
	adExisted := existed
	if existed {
		*steps = append(*steps, fmt.Sprintf("Ad already exists (%s) with the same destination (not re-created)", adID))
	} else {
		*steps = append(*steps, fmt.Sprintf("Ad created: %s (PAUSED, ResponsiveSearch)", adID))
	}

	r := adGroupWithIDPartial()
	r.AdID = adID
	// AlreadyExisted is true only when this run created NOTHING — i.e. the campaign, the
	// ad group, AND the ad were all pre-existing. If any level was created this run, the
	// run did produce something new, so the field must be false (its documented contract).
	r.AlreadyExisted = alreadyExisted && adGroupExisted && adExisted
	r.Steps = *steps
	return r, nil
}

// findOrCreateAdGroup returns (id, existed, err). It first looks the ad group up by
// name under the campaign (a read), returning it if present; otherwise it POSTs the ad
// group to /AdGroups with the CampaignId in the body. Ad-group names are unique within a
// campaign, so the name lookup is the idempotency key (a stable name → a retry reuses
// the existing group).
func (c *Client) findOrCreateAdGroup(ctx context.Context, campaignID, name string) (id string, existed bool, err error) {
	if existingID, ferr := c.findAdGroupByName(ctx, campaignID, name); ferr != nil {
		return "", false, ferr
	} else if existingID != "" {
		return existingID, true, nil
	}
	req := createAdGroupsRequest{
		CampaignId: json.Number(campaignID),
		AdGroups: []msAdGroup{{
			Name:        name,
			Status:      adGroupStatusPaused,
			AdGroupType: adGroupTypeSearchStandard,
			Language:    adGroupLanguage,
		}},
	}
	body, err := c.doRequest(ctx, http.MethodPost, "AdGroups", req, false)
	if err != nil {
		return "", false, err
	}
	var resp createAdGroupsResponse
	newID, err := firstEntityID(body, "AdGroupIds", func(b []byte) ([]*json.Number, []msErrorItem, error) {
		if uErr := json.Unmarshal(b, &resp); uErr != nil {
			return nil, nil, uErr
		}
		return resp.AdGroupIds, resp.PartialErrors, nil
	})
	if err != nil {
		return "", false, err
	}
	return newID, false, nil
}

// findAdGroupByName returns the id of the ad group whose Name matches name (case-
// insensitively, per Microsoft's uniqueness comparison) under the campaign, or "" if
// none. It POSTs /AdGroups/QueryByCampaignId with the CampaignId in the body (the v13
// GetAdGroupsByCampaignId REST operation is a POST-with-body, not a GET). A READ
// (idempotent, retried on 429); the response carries the full set for the campaign in
// one response (not paged), so the single-shot read can't miss a match.
func (c *Client) findAdGroupByName(ctx context.Context, campaignID, name string) (string, error) {
	req := queryAdGroupsRequest{CampaignId: json.Number(campaignID)}
	body, err := c.doRequest(ctx, http.MethodPost, "AdGroups/QueryByCampaignId", req, true)
	if err != nil {
		return "", err
	}
	var resp queryAdGroupsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		return "", fmt.Errorf("decode AdGroups/QueryByCampaignId response: %w", uErr)
	}
	for _, g := range resp.AdGroups {
		if strings.EqualFold(g.Name, name) {
			if id := numberID(g.Id); id != "" {
				return id, nil
			}
		}
	}
	return "", nil
}

// findOrCreateResponsiveSearchAd returns (id, existed, err). It looks for an existing ad
// in the ad group whose FinalUrls already contains finalURL (ads have no stable name, so
// the destination URL is the idempotency key — and v13 ALLOWS duplicate responsive search
// ads in an ad group, so this find-first is what keeps a retry from stacking duplicates),
// returning it if present; otherwise it creates a PAUSED ResponsiveSearchAd with the given
// headline/description assets.
func (c *Client) findOrCreateResponsiveSearchAd(ctx context.Context, adGroupID string, headlines, descriptions []string, finalURL string) (id string, existed bool, err error) {
	if existingID, ferr := c.findAdByFinalURL(ctx, adGroupID, finalURL); ferr != nil {
		return "", false, ferr
	} else if existingID != "" {
		return existingID, true, nil
	}
	req := createAdsRequest{
		AdGroupId: json.Number(adGroupID),
		Ads: []msResponsiveSearchAd{{
			Type:         adTypeResponsiveSearch,
			Headlines:    textAssetLinks(headlines),
			Descriptions: textAssetLinks(descriptions),
			FinalUrls:    []string{finalURL},
			Status:       adStatusPaused,
		}},
	}
	body, err := c.doRequest(ctx, http.MethodPost, "Ads", req, false)
	if err != nil {
		return "", false, err
	}
	var resp createAdsResponse
	newID, err := firstEntityID(body, "AdIds", func(b []byte) ([]*json.Number, []msErrorItem, error) {
		if uErr := json.Unmarshal(b, &resp); uErr != nil {
			return nil, nil, uErr
		}
		return resp.AdIds, resp.PartialErrors, nil
	})
	if err != nil {
		return "", false, err
	}
	return newID, false, nil
}

// textAssetLinks wraps each string as a TextAsset inside an AssetLink for the Headlines/
// Descriptions lists of a ResponsiveSearchAd.
func textAssetLinks(texts []string) []msAssetLink {
	links := make([]msAssetLink, 0, len(texts))
	for _, t := range texts {
		links = append(links, msAssetLink{Asset: msTextAsset{Type: "TextAsset", Text: t}})
	}
	return links
}

// queryAdsResponse is the (subset of the) Ads/QueryByAdGroupId response used to match an
// existing ad by its destination for idempotency.
type queryAdsResponse struct {
	Ads []struct {
		Id        *json.Number `json:"Id"`
		FinalUrls []string     `json:"FinalUrls"`
	} `json:"Ads"`
}

// findAdByFinalURL returns the id of an ad in the group whose FinalUrls contains
// finalURL, or "" if none. It POSTs /Ads/QueryByAdGroupId with the AdGroupId in the body
// (the v13 GetAdsByAdGroupId REST operation is a POST-with-body, not a GET). A READ
// (idempotent). Matching on the destination keeps a retry from stacking duplicate ads
// (ads have no stable name to key on, and v13 permits duplicate responsive search ads).
func (c *Client) findAdByFinalURL(ctx context.Context, adGroupID, finalURL string) (string, error) {
	req := queryAdsRequest{AdGroupId: json.Number(adGroupID), AdTypes: []string{adTypeResponsiveSearch}}
	body, err := c.doRequest(ctx, http.MethodPost, "Ads/QueryByAdGroupId", req, true)
	if err != nil {
		return "", err
	}
	var resp queryAdsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		return "", fmt.Errorf("decode Ads/QueryByAdGroupId response: %w", uErr)
	}
	for _, ad := range resp.Ads {
		if ad.Id == nil {
			continue
		}
		for _, u := range ad.FinalUrls {
			if u == finalURL {
				if id := numberID(ad.Id); id != "" {
					return id, nil
				}
			}
		}
	}
	return "", nil
}

// errNoID marks a 2xx create response that carried neither a usable id NOR a
// PartialError explaining a rejection — a malformed success. The mutation MAY have
// committed upstream, so createAdGroupAndAd classifies it as UNCONFIRMED (verify before
// retry), never as a clean failure. firstCampaignID's caller reaches the same UNCONFIRMED
// outcome via an explicit else; the ad-group/ad path keys off this sentinel because its
// call sites branch on createOutcomeAmbiguous first.
var errNoID = errors.New("create response carried no id")

// firstEntityID decodes a create-entities 200 body via extract (which returns the
// id slice + PartialErrors) and returns the created id. It mirrors firstCampaignID's
// contract exactly: a valid first id is success; a null id slot WITH an ACTUAL
// PartialError is a definite rejection (errPartialFailure); anything else (no id, no
// real error) is a malformed 200 → errNoID, which the caller treats as UNCONFIRMED.
// Extracted so the ad-group and ad creates share one classification path.
//
// firstEntityID only runs on a 2xx body, so an UNPARSEABLE body is also errNoID-ambiguous:
// the create may have committed but the result is unreadable, so a blind retry could
// duplicate — it must NOT be reported as a clean failure.
func firstEntityID(body []byte, idField string, extract func([]byte) ([]*json.Number, []msErrorItem, error)) (string, error) {
	ids, partials, err := extract(body)
	if err != nil {
		return "", fmt.Errorf("decode %s response (%v): %w", idField, err, errNoID)
	}
	if len(ids) > 0 && ids[0] != nil {
		if id := numberID(ids[0]); id != "" {
			return id, nil
		}
	}
	// No valid id. Only an ACTUAL PartialError is a definite rejection; a PartialErrors
	// slice of nothing but null placeholders (position-aligned with the id slots) does
	// NOT explain a failure, so it must fall through to UNCONFIRMED — mirroring
	// firstCampaignID. len(partials) would wrongly treat a null-only slice as a rejection.
	if partialErrorsHaveAny(partials) {
		return "", fmt.Errorf("%w: %s", errPartialFailure, partialErrorCodes(partials))
	}
	return "", fmt.Errorf("%s %w", idField, errNoID)
}

// composeAdGroupName builds a deterministic ad-group name from the input, mirroring
// composeName's sanitization so a retry composes the same name (the idempotency key).
func composeAdGroupName(in CampaignInput) string {
	parts := []string{"LFX", "Ad Group"}
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

// composeAdCopy returns the (headlines, descriptions) asset lists for the responsive
// search ad, each already de-duplicated (case-insensitively), rune-truncated to its field
// limit, and padded to Microsoft's REQUIRED minimum count (>=3 headlines, >=2 descriptions)
// with deterministic placeholders derived from EventName/Project — a safe PAUSED default a
// human edits before enabling. Caller-supplied entries (validated separately) come first.
// The lists are also capped at the maximum (15 / 4). The ad is PAUSED, so a truncated
// placeholder is acceptable and can't fail the paid create.
func composeAdCopy(in CampaignInput) (headlines, descriptions []string) {
	event := sanitizeNamePart(in.EventName)
	project := sanitizeNamePart(in.Project)

	// Headline candidates: caller-supplied first, then deterministic fallbacks. join()
	// drops empty segments so a missing Project doesn't yield "Register for  ".
	hCandidates := append([]string{}, in.Headlines...)
	hCandidates = append(hCandidates,
		event,
		join(" | ", project, event),
		join(" ", "Register for", event),
		"Register Today",
		"Learn More",
		"Join Us",
	)
	dCandidates := append([]string{}, in.Descriptions...)
	dCandidates = append(dCandidates,
		join(" ", "Learn more about", event)+".",
		join(" ", "Register now for", event, pfx("by ", project))+".",
		join(" ", "Join us for", event)+".",
	)

	headlines = boundedUniqueCopy(hCandidates, maxAdHeadlineRunes, maxAdHeadlineRunesWide, minAdHeadlines, maxAdHeadlines)
	descriptions = boundedUniqueCopy(dCandidates, maxAdDescriptionRunes, maxAdDescriptionRunesWide, minAdDescriptions, maxAdDescriptions)
	return headlines, descriptions
}

// hasDoubleWidth reports whether s contains any character Microsoft treats as double-width
// (CJK / Korean / Japanese / Chinese ideographs, or an emoji), which halves the allowed
// asset length. A conservative over-detection is harmless here (it only tightens the cap).
func hasDoubleWidth(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x11FF, // Hangul Jamo
			r >= 0x2E80 && r <= 0x9FFF, // CJK radicals … unified ideographs
			r >= 0xAC00 && r <= 0xD7AF, // Hangul syllables
			r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
			r >= 0xFF00 && r <= 0xFFEF, // full-width forms
			r >= 0x1F000:               // emoji / supplementary symbol planes
			return true
		}
	}
	return false
}

// adCopyLimit returns the per-string rune limit: the reduced wide limit when s contains any
// double-width character, else the single-width limit.
func adCopyLimit(s string, single, wide int) int {
	if hasDoubleWidth(s) {
		return wide
	}
	return single
}

// boundedUniqueCopy trims each candidate, truncates it to its WIDTH-AWARE limit (single or
// wide), keeps only non-empty, case-insensitively-unique entries in order, caps the result
// at maxCount, and — if fewer than minCount survive — pads with numbered "Learn More N"
// placeholders so the required minimum is always met (the ad is PAUSED, so a placeholder is
// a safe default).
func boundedUniqueCopy(candidates []string, singleLimit, wideLimit, minCount, maxCount int) []string {
	out := make([]string, 0, maxCount)
	seen := make(map[string]struct{}, maxCount)
	add := func(s string) bool {
		s = strings.TrimSpace(s)
		s = truncateRunes(s, adCopyLimit(s, singleLimit, wideLimit))
		if s == "" {
			return false
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		out = append(out, s)
		return len(out) >= maxCount
	}
	for _, c := range candidates {
		if add(c) {
			return out
		}
	}
	// Pad deterministically to the minimum with distinct placeholders.
	for n := 1; len(out) < minCount; n++ {
		if add(fmt.Sprintf("Learn More %d", n)) {
			break
		}
	}
	return out
}

// join concatenates the non-empty segments with sep (so a missing segment never leaves a
// doubled separator or trailing sep).
func join(sep string, segs ...string) string {
	kept := make([]string, 0, len(segs))
	for _, s := range segs {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, sep)
}

// pfx returns prefix+s when s is non-empty, else "" (so an optional segment like
// "by <project>" vanishes cleanly when project is empty).
func pfx(prefix, s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return prefix + s
}

// validateAdCopy checks caller-supplied headline/description entries against the responsive
// search ad limits BEFORE any create: count cap, per-entry WIDTH-AWARE rune cap (30/90, or
// 15/45 for double-width copy), case-insensitive uniqueness, at least one word, and no
// newline character (all Microsoft RSA content rules). Empty/short lists are fine —
// composeAdCopy pads them to the minimum. A violation is a clean up-front (nil, err) so bad
// caller copy never orphans a PAUSED campaign/ad group behind a create Microsoft rejects.
func validateAdCopy(in CampaignInput) error {
	if err := checkAdCopyList("headline", in.Headlines, maxAdHeadlines, maxAdHeadlineRunes, maxAdHeadlineRunesWide); err != nil {
		return err
	}
	return checkAdCopyList("description", in.Descriptions, maxAdDescriptions, maxAdDescriptionRunes, maxAdDescriptionRunesWide)
}

// checkAdCopyList validates one caller list. Empty entries are ignored (composeAdCopy skips
// them); every non-empty entry must contain at least one word, no newline, be within its
// width-aware rune cap, and be case-insensitively unique. Checks apply to the trimmed value
// the ad will actually carry.
func checkAdCopyList(kind string, items []string, maxCount, singleLimit, wideLimit int) error {
	if n := len(items); n > maxCount {
		return fmt.Errorf("at most %d %ss are allowed, got %d", maxCount, kind, n)
	}
	seen := make(map[string]struct{}, len(items))
	for i, raw := range items {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.ContainsAny(raw, "\n\r") {
			return fmt.Errorf("%s %d must not contain a newline", kind, i+1)
		}
		if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
			return fmt.Errorf("%s %d must contain at least one word", kind, i+1)
		}
		if limit := adCopyLimit(s, singleLimit, wideLimit); utf8.RuneCountInString(s) > limit {
			return fmt.Errorf("%s %d exceeds %d characters", kind, i+1, limit)
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%s %d is a duplicate (case-insensitive): %q", kind, i+1, s)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// truncateRunes returns at most n runes of s (never splitting a multibyte rune).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// buildAdFinalURL returns the ad's destination: the registration URL with the LFX
// utm_* attribution params SET (replacing only those keys, preserving every other
// query param). Falls back to the raw URL if it can't be parsed (validateAdURL has
// already rejected a genuinely malformed URL, so this is defensive).
func buildAdFinalURL(in CampaignInput) string {
	base := strings.TrimSpace(in.RegistrationURL)
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("utm_source", "microsoft")
	q.Set("utm_medium", "cpc")
	if slug := sanitizeNamePart(in.EventSlug); slug != "" {
		q.Set("utm_campaign", slug)
	} else if e := sanitizeNamePart(in.EventName); e != "" {
		q.Set("utm_campaign", e)
	}
	if p := sanitizeNamePart(in.Project); p != "" {
		q.Set("utm_content", p)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// validateAdURL rejects an empty/malformed ad destination BEFORE any mutating call.
// https/http only, absolute, no embedded userinfo (an ad destination never needs URL
// credentials, and forwarding them would leak a secret), and a well-formed query (a
// malformed %-escape would be silently dropped by u.Query() in buildAdFinalURL,
// changing the destination). Mirrors the reddit client's validateRegistrationURL.
func validateAdURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("registration URL is required to create the ad")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// Do NOT wrap the url.Parse error: a *url.Error embeds the full raw URL,
		// re-exposing any userinfo/query secret even though we redacted the %q arg.
		return fmt.Errorf("registration URL %q is not a valid URL", redactAdURL(raw))
	}
	if !u.IsAbs() || u.Hostname() == "" {
		return fmt.Errorf("registration URL %q must be absolute (include scheme and host)", redactAdURL(raw))
	}
	if _, qerr := url.ParseQuery(u.RawQuery); qerr != nil {
		return fmt.Errorf("registration URL %q has a malformed query string", redactAdURL(raw))
	}
	if u.User != nil {
		return fmt.Errorf("registration URL must not contain embedded credentials (userinfo)")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", redactAdURL(raw), u.Scheme)
	}
}

// redactAdURL returns a URL safe to echo in an error: scheme://host/path only (query,
// fragment, and userinfo dropped). Mirrors the reddit/meta redactURL.
func redactAdURL(raw string) string {
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
	return truncate(trimmed, maxErrorBodyChars)
}
