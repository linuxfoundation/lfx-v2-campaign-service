// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/width"
)

// ---------------------------------------------------------------------------
// Ad group + ad creation (MS-2.5): complete the Campaign -> AdGroup -> Ad tree
//
// CreateCampaign creates the campaign (MS-2), then this file finishes the
// hierarchy so the result is a usable PAUSED campaign rather than an empty shell —
// mirroring the reddit/meta clients, whose CreateCampaign creates all three levels.
// Everything stays PAUSED so nothing serves until a human enables it.
//
// The PartialErrors-on-200 transport fact from campaign.go applies at every level: a
// per-entity failure is a 200 with a null id slot + a PartialError, inspected via
// firstEntityID. Idempotency, though, differs by level: campaign and ad-group names are
// UNIQUE (case-insensitive), so each is find-or-create by its deterministic name; an ad
// has no stable name, so it is find-or-create by its destination (FinalUrls). All three
// find-first lookups keep a SEQUENTIAL retry from stacking duplicates.
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

	// adGroupLanguage is the AdGroup.Language sent on create. Per v13, Language is the
	// TARGETING language — "the language of your customers", i.e. which searchers see the
	// ad — and is INDEPENDENT of the character set of the ad copy: it does NOT have to match
	// the headline/description text (this client may carry CJK copy under any targeting
	// language). Language is "Optional if the campaign has one or more languages set, and
	// otherwise required for most campaign types"; the MS-2 campaign create sets no
	// campaign-level Languages, so the ad group MUST carry one or AddAdGroups is rejected.
	// A Search ad group's Language must be a specific supported language string ("All" is
	// only valid for the CAMPAIGN Languages element on Audience/DSA campaigns, NOT a Search
	// ad group), so a concrete value is required. English is a valid, always-available
	// default for this PAUSED broker shell; because the campaign is PAUSED, a human sets the
	// correct customer-facing targeting language (e.g. TraditionalChinese) before enabling.
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

	// maxDisplayDomainRunes bounds the ad's DISPLAY domain, which Microsoft derives from the
	// FinalUrls hostname. Microsoft caps the displayed URL (domain plus optional Path1/Path2)
	// at 67 characters; the RSA build sets no Path1/Path2, so the whole budget is the hostname.
	// A hostname longer than this passes the 2,048-char FinalUrls check but is rejected only at
	// AddAds — after the campaign/ad group exist — so it is validated up front alongside the
	// FinalUrls length to keep a bad host from orphaning a PAUSED campaign.
	maxDisplayDomainRunes = 67

	// maxDisplayDomainRunesWide is the reduced display-URL cap "for languages with double-width
	// characters" (33 vs 67), per the same v13 ResponsiveSearchAd Path1/Path2 element docs. As
	// with the copy limits, v13 gives no per-character weighted formula, so a hostname
	// containing ANY double-width character (e.g. a CJK IDN) is conservatively held to this
	// reduced cap — never over-length, at worst rejecting a borderline wide host a little early.
	maxDisplayDomainRunesWide = 33

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
// explicitly for clarity).
//
// CpcBid is the ad group's max cost-per-click and is OMITTED when the caller supplies none
// (a nil pointer with omitempty). Omission is a documented, serve-capable state: Microsoft
// sets an unset bid to "the minimum depending on your account's currency". Sending an
// explicit {"Amount":0} instead would be a zero bid — a different and worse thing — which is
// why this is a POINTER rather than a float64 with omitempty semantics of its own.
//
// NOTE for a future change: Microsoft has IGNORED bid strategies on ad groups and keywords
// since April 2021 — "the request will be ignored without error" — so an AdGroup.BiddingScheme
// added here would be a silent no-op. The bid strategy is the CAMPAIGN's (v13 defaults a
// Search campaign to EnhancedCpcBiddingScheme), and CpcBid is the ad-group-level control that
// still does something.
type msAdGroup struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	AdGroupType string `json:"AdGroupType"`
	Language    string `json:"Language"`
	CpcBid      *msBid `json:"CpcBid,omitempty"`
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

// Both arrays are BOUNDED slice types (see boundedNumberIDs / boundedErrorItems) so a
// malformed up-to-8-MiB create response packed with null/empty entries can't amplify into tens
// of MiB of allocations per concurrent create — only the first id and whether ANY PartialError
// is present are ever needed. Mirrors createCampaignsResponse.
type createAdGroupsResponse struct {
	AdGroupIds    boundedNumberIDs  `json:"AdGroupIds"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
}

// queryAdGroupsRequest is the POST /AdGroups/QueryByCampaignId body used by
// findAdGroupByName — the v13 GetAdGroupsByCampaignId REST operation is a POST with the
// CampaignId in the body, not a GET.
type queryAdGroupsRequest struct {
	CampaignId json.Number `json:"CampaignId"`
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

// Bounded slice types, as createAdGroupsResponse — a malformed 8-MiB create body can't OOM.
type createAdsResponse struct {
	AdIds         boundedNumberIDs  `json:"AdIds"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
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

// entityState renders "found" for a pre-existing entity matched by lookup or "created"
// for one this run created, for accurate error/step text (so a retry against an existing
// hierarchy does not falsely attribute the side effect to this run).
func entityState(existed bool) string {
	if existed {
		return "found"
	}
	return "created"
}

// createAdGroupAndAd completes the hierarchy under an already-created/found campaign:
// it find-or-creates a PAUSED ad group, then creates a PAUSED Responsive Search Ad under it
// (v13 does not support adding TextAd). Each step accumulates its ids into the result so an
// ambiguous failure at a later step leaves the whole tree reconcilable, never orphaned
// anonymously.
//
// campaignPartial() returns the result carrying everything known so far (campaign id +
// name); this function extends it with the ad-group and ad ids/names as they land.
// targeting carries the ALREADY-VALIDATED targeting inputs from CreateCampaign into the
// ad-group/ad/keyword cascade. It exists so the values are validated exactly once, up front,
// before anything is created: re-deriving them here would let the check that prevents an
// orphaned tree drift away from the values actually sent.
//
// cpcBidSet distinguishes "no bid supplied" from "a bid of 0.0", which is the distinction
// that decides whether the CpcBid field is SENT at all. Without it, an unset bid would be
// serialized as {"Amount":0} — an explicit zero bid, which is not what Microsoft's
// documented "unset takes the account-currency minimum" behaviour does.
type targeting struct {
	keywords  []Keyword
	cpcBid    float64
	cpcBidSet bool
}

func (c *Client) createAdGroupAndAd(
	ctx context.Context,
	in CampaignInput,
	campaignID string,
	alreadyExisted bool,
	steps *[]string,
	campaignPartial func() *CampaignResult,
	tgt targeting,
) (*CampaignResult, error) {
	// The ad destination URL (in.RegistrationURL) is validated up front in CreateCampaign,
	// BEFORE the campaign create, so a bad URL fails cleanly without orphaning a PAUSED
	// campaign or ad group. No re-validation here: the input hasn't changed, and repeating
	// it would only risk the two checks drifting apart.

	// campaignState renders the campaign's provenance for error text: "created" only when
	// THIS run created it, else "found" (a pre-existing campaign matched by lookup on a
	// retry). Using "created" unconditionally would falsely attribute the side effect to
	// this run when the hierarchy was actually reused.
	campaignState := entityState(alreadyExisted)

	adGroupName := composeAdGroupName(in)
	if err := validateEntityName("ad group", adGroupName, utf8.RuneCountInString(adGroupName), maxAdGroupNameRunes, "characters"); err != nil {
		return campaignPartial(), fmt.Errorf("microsoft-ads ad group name invalid (campaign %s %s): %w", campaignID, campaignState, err)
	}

	// adGroupPartial carries the campaign id/name + the ad-group name (and, once known,
	// its id) so an ambiguous ad-group/ad failure is reconcilable.
	adGroupPartial := func() *CampaignResult {
		r := campaignPartial()
		r.AdGroupName = adGroupName
		return r
	}

	// If the context is ALREADY done before the ad-group step, abort cleanly BEFORE firing
	// any ad-group lookup/create HTTP work — the campaign id is known and returned in a
	// reconcilable partial (mirrors the pre-ad-step guard below), so a cancellation after the
	// campaign create can't still go on to mutate ad-group state.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return adGroupPartial(), fmt.Errorf("microsoft-ads ad group step aborted (campaign %s %s; context done before the ad-group step, no ad group created): %w", campaignID, campaignState, ctxErr)
	}

	// Step 3: find-or-create the ad group under the campaign. The lookup is a read
	// (idempotent), the create is a mutation (not retried on 429). A cancellation
	// during the lookup is a clean abort (nothing new created), but the CAMPAIGN
	// already exists, so it is surfaced as a reconcilable partial rather than (nil,err).
	adGroupID, existed, err := c.findOrCreateAdGroup(ctx, campaignID, adGroupName, tgt.cpcBid, tgt.cpcBidSet)
	if err != nil {
		// ORDER MATTERS. createOutcomeAmbiguous catches a transportError FIRST — a ctx-cancel
		// mid-HTTP-Do is wrapped as a transportError (whose Unwrap exposes context.Canceled),
		// and that create MAY have committed, so it must stay UNCONFIRMED. Only a BARE context
		// error (from the read lookup's backoff/pre-send, not wrapped in transportError) is a
		// clean abort where nothing was created. errNoID (malformed 2xx, no id) is UNCONFIRMED.
		switch {
		case createOutcomeAmbiguous(err) || errors.Is(err, errNoID):
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation UNCONFIRMED (campaign %s %s; %q may exist — verify before retrying): %w", campaignID, campaignState, adGroupName, err)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group step aborted (campaign %s %s; context done during the lookup, no ad group created): %w", campaignID, campaignState, err)
		case errors.Is(err, errPartialFailure):
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation rejected (campaign %s %s): %w", campaignID, campaignState, err)
		default:
			return adGroupPartial(), fmt.Errorf("microsoft-ads ad group creation failed (campaign %s %s): %w", campaignID, campaignState, err)
		}
	}
	adGroupExisted := existed
	// hierState renders the "campaign <id> <state> + ad group <id> <state>" prefix for the
	// ad-step error text, so "created" vs "found" is accurate per entity on a retry against
	// an existing hierarchy.
	hierState := fmt.Sprintf("campaign %s %s + ad group %s %s", campaignID, campaignState, adGroupID, entityState(adGroupExisted))
	if existed {
		*steps = append(*steps, fmt.Sprintf("Ad group already exists by name: %s (not re-created, bid unchanged)", adGroupID))
	} else if tgt.cpcBidSet {
		*steps = append(*steps, fmt.Sprintf("Ad group created: %s (PAUSED, CpcBid %.2f in the account currency)", adGroupID, tgt.cpcBid))
	} else {
		// Say what the ABSENT bid means, rather than staying silent about it: Microsoft applies
		// the account-currency minimum, and an operator reading these steps needs to know that
		// the campaign has a real (if minimal) bid rather than none at all.
		*steps = append(*steps, fmt.Sprintf("Ad group created: %s (PAUSED, no CpcBid set — Microsoft applies the account-currency minimum)", adGroupID))
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
		return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad step aborted (%s; context done before the ad step, no ad created): %w", hierState, ctxErr)
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
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation UNCONFIRMED (%s; an ad may exist — verify before retrying): %w", hierState, err)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad step aborted (%s; context done during the lookup, no ad created): %w", hierState, err)
		case errors.Is(err, errPartialFailure):
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation rejected (%s): %w", hierState, err)
		default:
			return adGroupWithIDPartial(), fmt.Errorf("microsoft-ads ad creation failed (%s): %w", hierState, err)
		}
	}
	adExisted := existed
	if existed {
		*steps = append(*steps, fmt.Sprintf("Ad already exists (%s) with the same destination (not re-created)", adID))
	} else {
		*steps = append(*steps, fmt.Sprintf("Ad created: %s (PAUSED, ResponsiveSearch)", adID))
	}

	adWithIDPartial := func() *CampaignResult {
		r := adGroupWithIDPartial()
		r.AdID = adID
		return r
	}

	// Step 5 (MS-4): attach the keywords. LAST, because it is the only step whose absence
	// leaves a tree that is complete but inert — every earlier step is a prerequisite for it,
	// and running it earlier would mean keywording an ad group whose ad might still fail.
	//
	// A REUSED ad group is NOT re-keyworded, and this is the one step where that rule has
	// teeth. v13's AddKeywords has NO idempotency key and there is no keyword READ operation
	// anywhere in this client — no GetKeywordsByAdGroupId, no list, nothing to reconcile
	// against — so on the reuse path the client cannot know which of these keywords already
	// hang off the group. Posting them "just in case" is therefore not a retry, it is a
	// SECOND COPY of every keyword: duplicate criteria, duplicate bids on the same terms,
	// and real duplicated spend the moment the campaign is enabled. Since the duplicate can
	// never be detected from here, the only safe direction is not to create it.
	//
	// This is the SAME rule findOrCreateAdGroup already applies one step earlier, where a
	// reused group keeps whatever CpcBid it has rather than being re-bid by a create-only
	// retry. Keywords are that group's other spend-bearing attribute, so leaving them alone
	// is the consistent choice, not a special case.
	//
	// THE COST, stated plainly rather than buried: a keyword ADDED to the brief between the
	// first run and this one will not be attached, so the campaign targets the older list.
	// That is the lesser harm by a wide margin — a missing keyword under-serves and is
	// visible in the steps below, whereas a duplicated keyword silently doubles the bid on a
	// term nobody re-approved. Attaching the new one is a human's call in the Bing UI (or a
	// real reconcile once a keyword read exists); over-spending is nobody's call.
	if adGroupExisted && len(tgt.keywords) > 0 {
		// The count reported is the number of SUPPLIED keywords that were not posted — NOT a
		// count of what the ad group already has, which this client has no way to read and must
		// not imply it knows.
		*steps = append(*steps, fmt.Sprintf(
			"Keywords NOT re-posted: ad group %s already existed, so its existing keywords were left unchanged and the %d supplied keyword(s) were not sent (v13 AddKeywords has no idempotency key and v13 exposes no keyword read, so re-posting would duplicate every keyword and double the bid on those terms). Any keyword added to the brief since the first run must be attached manually.",
			adGroupID, len(tgt.keywords)))

		r := adWithIDPartial()
		// AlreadyExisted follows its documented contract: true only when this run created
		// NOTHING. Reaching here means the ad group pre-existed and no keyword was posted, so
		// the campaign and ad levels alone decide it.
		r.AlreadyExisted = alreadyExisted && adExisted
		r.Steps = *steps
		return r, nil
	}

	// A pre-step context check, as at every other step: a cancelled context must not fire a
	// mutating create whose outcome would then be UNCONFIRMED.
	if len(tgt.keywords) > 0 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return adWithIDPartial(), fmt.Errorf("microsoft-ads keyword step aborted (%s + ad %s; context done before the keyword step, no keywords created): %w", hierState, adID, ctxErr)
		}
		keywordIDs, kerr := c.createKeywords(ctx, adGroupID, tgt.keywords)
		if kerr != nil {
			// Same ordered classification as the ad group and ad: an ambiguous transport error or
			// errNoID (malformed/short 2xx) stays UNCONFIRMED, because AddKeywords has NO
			// idempotency key — a blind retry would add a second copy of every keyword rather
			// than reconciling onto the first. Only a definite PartialError is a clean rejection.
			switch {
			case createOutcomeAmbiguous(kerr) || errors.Is(kerr, errNoID):
				return adWithIDPartial(), fmt.Errorf("microsoft-ads keyword targeting UNCONFIRMED (%s + ad %s; keywords may exist — verify before retrying, a blind retry would duplicate them): %w", hierState, adID, kerr)
			case errors.Is(kerr, context.Canceled) || errors.Is(kerr, context.DeadlineExceeded):
				return adWithIDPartial(), fmt.Errorf("microsoft-ads keyword step aborted (%s + ad %s; context done, no keywords created): %w", hierState, adID, kerr)
			case errors.Is(kerr, errPartialFailure):
				// A batch rejection can still have created some keywords, and createKeywords
				// returns their ids alongside the error. Persist them: they are what ACTIVATE
				// enables, and what stops a reconciliation creating a second copy of every
				// keyword that did succeed. Dropping them here reported "rejected" for keywords
				// that exist upstream.
				partial := adWithIDPartial()
				partial.KeywordIDs = keywordIDs
				if len(keywordIDs) > 0 {
					*steps = append(*steps, fmt.Sprintf("Keywords partially attached: %d created, some rejected (PAUSED)", len(keywordIDs)))
					partial.Steps = *steps
				}
				return partial, fmt.Errorf("microsoft-ads keyword targeting rejected (%s + ad %s): %w", hierState, adID, kerr)
			default:
				return adWithIDPartial(), fmt.Errorf("microsoft-ads keyword targeting failed (%s + ad %s): %w", hierState, adID, kerr)
			}
		}
		r := adWithIDPartial()
		r.KeywordIDs = keywordIDs
		// The two counts cannot diverge: createKeywords caps input at maxKeywords, decodes the id
		// array through a bound of the same size, and returns success only after finding one
		// usable id per keyword sent. This previously reported "ids PARSED" because the decode
		// bound was 16 and an oversized request legitimately came back short — that gap is what
		// LFXV2-3279 closed, so the count is now a confirmed one rather than a hedged one.
		*steps = append(*steps, fmt.Sprintf("Keywords attached: %d (PAUSED — enable them with the campaign)", len(keywordIDs)))
		r.AlreadyExisted = false // this run created keywords, so the tree is not untouched
		r.Steps = *steps
		return r, nil
	}

	// No keywords supplied. Say so explicitly in the steps: a campaign in this state is
	// structurally complete but can never serve, and an operator reading the result needs
	// that stated rather than inferred from a missing line.
	*steps = append(*steps, "No keywords supplied — this campaign cannot serve until keywords are added")

	r := adWithIDPartial()
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
// unconfirmedLookupErr classifies an idempotency-lookup failure the way the campaign level does
// (findCampaignByName): a lookup is a READ that confirms absence before the find-first create,
// so ANY failure that did not stem from the CALLER cancelling means we could NOT confirm the
// entity is absent — a blind create might duplicate one a prior attempt made (v13 ALLOWS
// duplicate RSAs, so this is sharp for ads). Such a failure is UNCONFIRMED, not a clean
// "creation failed", so it is folded onto errNoID (which createAdGroupAndAd's switch maps to
// UNCONFIRMED).
//
// The clean-abort test gates on the CALLER's ctx.Err(), NOT errors.Is(ferr, DeadlineExceeded):
// a token refresh runs under its own DETACHED timeout and surfaces a tokenTransportError
// wrapping context.DeadlineExceeded while the caller's context is still live (client.go). That
// is a FAILED lookup (absence unconfirmed), not a caller abort — matching a bare
// errors.Is(DeadlineExceeded) would mislabel it a clean abort and invite a duplicate. Only when
// the caller's own ctx is done is nothing-was-created true, so the error passes through for the
// switch's cancel branch. (A per-attempt client timeout already surfaces inside a transportError
// → createOutcomeAmbiguous → UNCONFIRMED, independent of this.)
func unconfirmedLookupErr(ctx context.Context, ferr error) error {
	if ctx.Err() != nil {
		return ferr // the caller cancelled/timed out — a clean abort; nothing created
	}
	if errors.Is(ferr, errNoID) {
		return ferr // already UNCONFIRMED-classified
	}
	return fmt.Errorf("idempotency lookup failed (cannot confirm the entity is absent; verify before retrying): %w: %w", ferr, errNoID)
}

func (c *Client) findOrCreateAdGroup(ctx context.Context, campaignID, name string, cpcBid float64, cpcBidSet bool) (id string, existed bool, err error) {
	if existingID, ferr := c.findAdGroupByName(ctx, campaignID, name); ferr != nil {
		return "", false, unconfirmedLookupErr(ctx, ferr)
	} else if existingID != "" {
		// A REUSED ad group keeps whatever bid it already has. This create-only path is not the
		// place to push a new CpcBid onto a group a previous run (or a human) already configured:
		// silently re-bidding an existing ad group would change what a live campaign pays on what
		// is meant to be an idempotent retry.
		return existingID, true, nil
	}
	adGroup := msAdGroup{
		Name:        name,
		Status:      adGroupStatusPaused,
		AdGroupType: adGroupTypeSearchStandard,
		Language:    adGroupLanguage,
	}
	if cpcBidSet {
		adGroup.CpcBid = &msBid{Amount: cpcBid}
	}
	req := createAdGroupsRequest{
		CampaignId: json.Number(campaignID),
		AdGroups:   []msAdGroup{adGroup},
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
		// A DUPLICATE-ad-group rejection (a race lost between the find-first lookup and this
		// create) means the group now EXISTS — reconcile by re-looking it up by name rather
		// than surfacing a hard failure (mirrors the campaign duplicate-name handling). Ad
		// group names are unique per campaign, so the re-lookup returns the winner's id.
		if isDuplicateAdGroupPartial(resp.PartialErrors) {
			existingID, ferr := c.findAdGroupByName(ctx, campaignID, name)
			if ferr == nil && existingID != "" {
				return existingID, true, nil
			}
			// Re-lookup failed/empty: the group exists but we can't confirm its id → UNCONFIRMED.
			// Surface the RE-LOOKUP cause (ferr) when it errored so operators can see WHY the id
			// couldn't be resolved (a 5xx, an auth failure, a timeout) — mirroring the
			// campaign-level self-heal. When the re-lookup succeeded but found no id, ferr is nil.
			if ferr != nil {
				return "", false, fmt.Errorf("ad group %q already exists but the reconciliation lookup failed (%v): %w", name, ferr, errNoID)
			}
			return "", false, fmt.Errorf("ad group %q already exists but could not be re-resolved (reconciliation lookup returned no id): %w", name, errNoID)
		}
		return "", false, err
	}
	return newID, false, nil
}

// errCodeDuplicateAdGroup is Microsoft's PartialError code when an ad-group name already
// exists in the campaign — the string ErrorCode enum or the equivalent numeric Code 1214.
const (
	errCodeDuplicateAdGroup        = "CampaignServiceCannotCreateDuplicateAdGroup"
	errCodeDuplicateAdGroupNumeric = "1214"
)

// isDuplicateAdGroupPartial reports whether a PartialErrors array carries the
// duplicate-ad-group rejection under either the symbolic ErrorCode enum or the numeric 1214.
func isDuplicateAdGroupPartial(items []msErrorItem) bool {
	return partialErrorsHaveCode(items, errCodeDuplicateAdGroup) ||
		partialErrorsHaveCode(items, errCodeDuplicateAdGroupNumeric)
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
	// STREAM the AdGroups array by name (see lookupNamedEntity) rather than Unmarshaling the
	// whole set: a malformed up-to-8-MiB body packed with `{}` entries would otherwise amplify
	// into tens of MiB per concurrent create. The omitted/null-vs-present distinction is
	// preserved and validated (a truncated array fails closed), mirroring lookupCampaignByName.
	id, matched, present, err := lookupNamedEntity(body, "AdGroups", name)
	if err != nil {
		return "", fmt.Errorf("decode AdGroups/QueryByCampaignId response (%v): %w", err, errNoID)
	}
	if !present {
		// The response OMITTED (or nulled, or truncated) the AdGroups field: we cannot confirm
		// the ad group is absent, and a blind create could duplicate. Fail closed as UNCONFIRMED.
		return "", fmt.Errorf("AdGroups/QueryByCampaignId response omitted the AdGroups field; cannot confirm ad group %q is absent: %w", name, errNoID)
	}
	if matched && id == "" {
		// The name matched (ad-group names are case-insensitively unique under a campaign) but
		// the id is null/unparseable: the group almost certainly exists. Reporting "" (absent)
		// would issue POST /AdGroups and create a DUPLICATE. errNoID → UNCONFIRMED.
		return "", fmt.Errorf("ad group %q found with no usable id: %w", name, errNoID)
	}
	return id, nil
}

// lookupNamedEntity streams the top-level object's `field` array (whose elements are
// {"Id":..,"Name":..}) comparing each Name case-insensitively to `name`, WITHOUT materializing
// the whole array. It returns the first match's usable id (or "" when the match has no usable
// id), whether a match was found, and whether the field was PRESENT as a well-formed, fully
// closed array. A truncated/unterminated array errors (fail closed), and an omitted/null field
// yields present=false. Shared by findAdGroupByName; mirrors lookupCampaignByName exactly.
func lookupNamedEntity(body []byte, field, name string) (id string, matched, present bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", false, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false, false, fmt.Errorf("expected a JSON object")
	}
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return "", false, false, kerr
		}
		if key, _ := keyTok.(string); key != field {
			if serr := skipJSONValue(dec); serr != nil {
				return "", false, false, serr
			}
			continue
		}
		vTok, verr := dec.Token()
		if verr != nil {
			return "", false, false, verr
		}
		if vTok == nil { // null → omitted
			return "", false, false, nil
		}
		if d, ok := vTok.(json.Delim); !ok || d != '[' {
			return "", false, false, fmt.Errorf("expected a JSON array for %s", field)
		}
		for dec.More() {
			var e struct {
				Id   *json.Number `json:"Id"`
				Name string       `json:"Name"`
			}
			if derr := dec.Decode(&e); derr != nil {
				return "", false, false, derr
			}
			if matched || !strings.EqualFold(e.Name, name) {
				continue
			}
			matched = true
			id = numberID(e.Id)
		}
		endTok, eerr := dec.Token() // must consume+validate the closing ']' (fail closed on truncation)
		if eerr != nil {
			return "", false, false, eerr
		}
		if d, ok := endTok.(json.Delim); !ok || d != ']' {
			return "", false, false, fmt.Errorf("malformed %s array (unterminated)", field)
		}
		// The array closed, but the ENCLOSING OBJECT must also be well-formed to trust the
		// result: a truncated body like `{"%s":[]` has a valid array yet an unterminated object,
		// and reporting a clean "present, no match" there would let the paid create run on an
		// unverified body. finishObject drains the remaining keys and confirms the closing '}'.
		if ferr := finishObject(dec); ferr != nil {
			return "", false, false, ferr
		}
		return id, matched, true, nil
	}
	return "", false, false, nil // no such key → omitted
}

// finishObject consumes the remaining key/value pairs of the CURRENT JSON object and validates
// its closing '}', erroring on a truncated/malformed remainder. It is called after the sought
// array has been fully read, so a truncated enclosing object fails closed rather than being
// trusted as a complete lookup response.
func finishObject(dec *json.Decoder) error {
	for dec.More() {
		if _, kerr := dec.Token(); kerr != nil { // key
			return kerr
		}
		if verr := skipJSONValue(dec); verr != nil { // value
			return verr
		}
	}
	endTok, eerr := dec.Token()
	if eerr != nil {
		return eerr
	}
	if d, ok := endTok.(json.Delim); !ok || d != '}' {
		return fmt.Errorf("malformed response object (unterminated)")
	}
	// Require EOF after the closing '}'. The decoder spans the WHOLE response body, so any
	// trailing bytes — junk appended to a truncated stream, or a second concatenated JSON
	// value — mean the body is malformed. Without this check `{"Ads":[]}garbage` would be
	// trusted as a clean empty lookup and the caller would create a duplicate, breaking the
	// fail-closed idempotency contract this helper exists to enforce. Insignificant trailing
	// whitespace is fine: json.Decoder skips it, so a well-formed body reports io.EOF here.
	if _, terr := dec.Token(); terr != io.EOF {
		if terr != nil {
			return terr
		}
		return fmt.Errorf("malformed response object (trailing data after object)")
	}
	return nil
}

// findOrCreateResponsiveSearchAd returns (id, existed, err). It looks for an existing ad
// in the ad group whose FinalUrls already contains finalURL (ads have no stable name, so
// the destination URL is the idempotency key — and v13 ALLOWS duplicate responsive search
// ads in an ad group, so this find-first is what keeps a retry from stacking duplicates),
// returning it if present; otherwise it creates a PAUSED ResponsiveSearchAd with the given
// headline/description assets.
//
// The find-first/create is idempotent across SEQUENTIAL retries only; it is not a
// concurrency guard, so two simultaneous creates for the same (adGroupID, finalURL) could
// both miss the lookup and both add an ad. That is not exposed here: like the sibling
// platform clients, concurrent dispatch for one campaign brief is serialized upstream by
// the orchestrator's claim contract (a single in-flight dispatch per campaign), so this
// path is never entered concurrently for the same destination.
func (c *Client) findOrCreateResponsiveSearchAd(ctx context.Context, adGroupID string, headlines, descriptions []string, finalURL string) (id string, existed bool, err error) {
	if existingID, ferr := c.findAdByFinalURL(ctx, adGroupID, finalURL); ferr != nil {
		return "", false, unconfirmedLookupErr(ctx, ferr)
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

// findAdByFinalURL returns the id of an ad in the group whose FinalUrls contains
// finalURL, or "" if none. It POSTs /Ads/QueryByAdGroupId with the AdGroupId in the body
// (the v13 GetAdsByAdGroupId REST operation is a POST-with-body, not a GET). A READ
// (idempotent). Matching on the destination keeps a retry from stacking duplicate ads
// (ads have no stable name to key on, and v13 permits duplicate responsive search ads).
//
// An ad whose FinalUrls MATCHES the target but whose Id is nil or unparseable is NOT
// treated as absent: the ad almost certainly exists (its destination matched) but its id
// is unreadable, so returning "" would let the caller create a SECOND ad for the same
// destination (v13 permits duplicate RSAs). That is reported as errNoID (UNCONFIRMED) so
// the caller verifies before retrying rather than blindly duplicating.
func (c *Client) findAdByFinalURL(ctx context.Context, adGroupID, finalURL string) (string, error) {
	req := queryAdsRequest{AdGroupId: json.Number(adGroupID), AdTypes: []string{adTypeResponsiveSearch}}
	body, err := c.doRequest(ctx, http.MethodPost, "Ads/QueryByAdGroupId", req, true)
	if err != nil {
		return "", err
	}
	// STREAM the Ads array matching on the destination, rather than Unmarshaling the whole set:
	// a malformed up-to-8-MiB body packed with `{}` entries would otherwise amplify into tens of
	// MiB per concurrent create. The omitted/null-vs-present distinction is preserved+validated
	// (a truncated array fails closed), mirroring lookupCampaignByName.
	id, matched, present, err := lookupAdByFinalURL(body, finalURL)
	if err != nil {
		return "", fmt.Errorf("decode Ads/QueryByAdGroupId response (%v): %w", err, errNoID)
	}
	if !present {
		// The response OMITTED (or nulled, or truncated) the Ads field: we cannot confirm the ad
		// is absent. v13 permits duplicate RSAs with no create-time reconcile, so treating a
		// missing field as "no ad" would stack a duplicate on retry. Fail closed as UNCONFIRMED.
		return "", fmt.Errorf("Ads/QueryByAdGroupId response omitted the Ads field; cannot confirm the ad for %q is absent: %w", redactAdURL(finalURL), errNoID)
	}
	if matched && id == "" {
		// Destination matched but the id is nil/unparseable: the ad exists yet we cannot key on
		// it. Ambiguous — do not report "absent".
		return "", fmt.Errorf("ad for %q found with no usable id: %w", redactAdURL(finalURL), errNoID)
	}
	return id, nil
}

// lookupAdByFinalURL streams the Ads/QueryByAdGroupId body, matching each ad's FinalUrls against
// finalURL on a CANONICAL form (canonicalFinalURL folds representation-only differences —
// param order, %-escape casing, default port, host case — so Microsoft re-encoding the stored
// URL on read-back still matches; a byte-exact compare would miss and stack a duplicate RSA).
// It never materializes the whole Ads set, preserves+validates the omitted/null-vs-present
// distinction (truncated → error, fail closed), and returns the first matching ad's usable id
// (or "" when the match has no usable id).
func lookupAdByFinalURL(body []byte, finalURL string) (id string, matched, present bool, err error) {
	wantKey := canonicalFinalURL(finalURL)
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", false, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false, false, fmt.Errorf("expected a JSON object")
	}
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return "", false, false, kerr
		}
		if key, _ := keyTok.(string); key != "Ads" {
			if serr := skipJSONValue(dec); serr != nil {
				return "", false, false, serr
			}
			continue
		}
		vTok, verr := dec.Token()
		if verr != nil {
			return "", false, false, verr
		}
		if vTok == nil { // null → omitted
			return "", false, false, nil
		}
		if d, ok := vTok.(json.Delim); !ok || d != '[' {
			return "", false, false, fmt.Errorf("expected a JSON array for Ads")
		}
		for dec.More() {
			var ad struct {
				Id        *json.Number `json:"Id"`
				FinalUrls []string     `json:"FinalUrls"`
			}
			if derr := dec.Decode(&ad); derr != nil {
				return "", false, false, derr
			}
			if matched {
				continue
			}
			for _, u := range ad.FinalUrls {
				if u == finalURL || canonicalFinalURL(u) == wantKey {
					matched = true
					id = numberID(ad.Id)
					break
				}
			}
		}
		endTok, eerr := dec.Token() // consume+validate the closing ']' (fail closed on truncation)
		if eerr != nil {
			return "", false, false, eerr
		}
		if d, ok := endTok.(json.Delim); !ok || d != ']' {
			return "", false, false, fmt.Errorf("malformed Ads array (unterminated)")
		}
		// Validate the enclosing object closes too (a truncated `{"Ads":[]` must fail closed).
		if ferr := finishObject(dec); ferr != nil {
			return "", false, false, ferr
		}
		return id, matched, true, nil
	}
	return "", false, false, nil // no Ads key → omitted
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

	// Deterministic fallbacks, used ONLY to pad below-minimum copy (not to augment
	// caller-authored copy). join() drops empty segments so a missing Project doesn't yield
	// "Register for  ".
	hFallbacks := []string{
		event,
		join(" | ", project, event),
		join(" ", "Register for", event),
		"Register Today",
		"Learn More",
		"Join Us",
	}
	dFallbacks := []string{
		join(" ", "Learn more about", event) + ".",
		join(" ", "Register now for", event, pfx("by ", project)) + ".",
		join(" ", "Join us for", event) + ".",
	}

	// Caller-supplied copy is used AS-IS (deduped + bounded); the fallbacks are appended only
	// as padding candidates, so a caller who supplies >= the minimum keeps EXACTLY their copy
	// (never silently augmented up to the maximum). The trailing len(in.Headlines)/len(in.Descriptions)
	// argument tells boundedUniqueCopy how many leading candidates are caller-authored: those are
	// always kept (up to maxCount), while the fallbacks that follow are consumed only while the
	// list is still below minCount. This honors the CampaignInput contract: explicit copy overrides
	// the auto-composition, and fallbacks only fill a shortfall to the minimum.
	headlines = boundedUniqueCopy(append(append([]string{}, in.Headlines...), hFallbacks...),
		maxAdHeadlineRunes, maxAdHeadlineRunesWide, minAdHeadlines, maxAdHeadlines, len(in.Headlines))
	descriptions = boundedUniqueCopy(append(append([]string{}, in.Descriptions...), dFallbacks...),
		maxAdDescriptionRunes, maxAdDescriptionRunesWide, minAdDescriptions, maxAdDescriptions, len(in.Descriptions))
	return headlines, descriptions
}

// hasDoubleWidth reports whether s contains any character Microsoft treats as double-width
// (CJK / Korean / Japanese / Chinese ideographs, or an emoji).
//
// Microsoft's documented rule is language-scoped: "for languages with double-width
// characters" a headline is capped at 15 final chars (vs 30) and a description at 45 (vs
// 90) — see the ResponsiveSearchAd Headlines/Descriptions element docs. The v13 REST
// contract does NOT publish a per-character weighted formula, so rather than guess one we
// apply the reduced 15/45 cap whenever ANY double-width character is present. This is
// deliberately conservative: it may truncate otherwise-valid mixed ASCII+wide copy a hair
// shorter than strictly required, but it can never emit an asset LONGER than Microsoft
// accepts (which would fail the ad AFTER its parent campaign/ad group were created). The
// copy composed here is auto-generated marketing text on a PAUSED shell, so a slightly
// tighter bound is an acceptable trade for guaranteed acceptance.
//
// Classification uses the Unicode East Asian Width property (golang.org/x/text/width)
// rather than a hand-maintained range list: the previous hardcoded ranges MISSED wide
// symbols outside them (⌚ U+231A, ⏰ U+23F0, ⬛ U+2B1B, ⭐ U+2B50, Hangul Jamo Ext-A)
// — which would then wrongly get the 30-rune cap and break this function's own
// "never longer than Microsoft accepts" guarantee — while being OVER-inclusive on common
// narrow BMP symbols (★ U+2605, ✓ U+2713 are East-Asian-Ambiguous/Neutral, i.e. single
// width in a Latin context) and hard-rejecting otherwise-valid caller copy. The width
// table folds all of those correctly. Emoji are handled separately: many render
// double-width but carry a Neutral/Ambiguous width property, so any codepoint in an emoji
// plane, plus the VS16 emoji-presentation selector (which promotes a preceding BMP glyph
// to a wide emoji), is treated as wide regardless of its width-table kind.
func hasDoubleWidth(s string) bool {
	for _, r := range s {
		if isEmojiWidth(r) {
			return true
		}
		switch width.LookupRune(r).Kind() {
		case width.EastAsianWide, width.EastAsianFullwidth:
			return true
		}
	}
	return false
}

// isEmojiWidth reports whether r is an emoji-plane codepoint (or the VS16 emoji-presentation
// selector) that renders double-width even though its East Asian Width property may be
// Neutral or Ambiguous. Kept as an explicit supplement to the width table in hasDoubleWidth.
//
// It deliberately does NOT blanket the BMP Misc-Symbols/Dingbats block (U+2600–U+27BF):
// those symbols are TEXT-presentation (single width) by default in a Latin context — star
// ★ U+2605 and check ✓ U+2713 among them — and only render wide when explicitly promoted
// by a following VS16, which the 0xFE0F case already catches. Blanketing the whole block
// would re-introduce the over-inclusive hard-rejection of otherwise-valid caller copy.
func isEmojiWidth(r rune) bool {
	switch {
	case r == 0xFE0F, // VS16 — promotes a preceding BMP glyph to a wide emoji
		r >= 0x1F000 && r <= 0x1FAFF: // emoji / supplementary symbol & pictograph planes
		return true
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

// hasWord reports whether s contains at least one letter or number — the Microsoft RSA
// rule that every headline/description asset must carry an actual word (punctuation-only
// content is rejected). Shared by checkAdCopyList (caller copy) and boundedUniqueCopy
// (auto-composed copy) so both paths enforce the same rule.
func hasWord(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	})
}

// boundedUniqueCopy trims each candidate, truncates it to its WIDTH-AWARE limit (single or
// wide), keeps only non-empty, word-bearing, case-insensitively-unique entries in order, and
// caps the result at maxCount. The FIRST callerCount candidates are the caller-authored copy
// and are kept AS-IS (up to maxCount); the remaining candidates are auto-composed FALLBACKS
// that are consumed ONLY to pad the list up to minCount — so a caller who supplies at least
// minCount entries gets exactly their copy, never silently augmented with auto-composed
// headlines/descriptions up to the maximum. If a shortfall remains after the fallbacks, it is
// filled with numbered "Learn More N" placeholders (the ad is PAUSED, so a placeholder is a
// safe default). The word check mirrors checkAdCopyList so an auto-composed asset (e.g. a
// sanitized EventName that is all punctuation) can't reach AddAds and orphan a PAUSED campaign.
func boundedUniqueCopy(candidates []string, singleLimit, wideLimit, minCount, maxCount, callerCount int) []string {
	out := make([]string, 0, maxCount)
	add := func(s string) bool {
		s = strings.TrimSpace(s)
		s = truncateRunes(s, adCopyLimit(s, singleLimit, wideLimit))
		if s == "" || !hasWord(s) {
			return false
		}
		// EqualFold (not a ToLower map key) so Unicode case variants like Σ/ς
		// are treated as duplicates; the list is capped at maxCount so the
		// linear scan is bounded.
		for _, kept := range out {
			if strings.EqualFold(kept, s) {
				return false
			}
		}
		out = append(out, s)
		return len(out) >= maxCount
	}
	for i, c := range candidates {
		// A fallback candidate (index >= callerCount) is only used to pad a shortfall: once the
		// minimum is met, stop consuming fallbacks so caller copy is never augmented past what
		// they authored. Caller entries (index < callerCount) are always added (up to maxCount).
		if i >= callerCount && len(out) >= minCount {
			break
		}
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

// checkAdCopyList validates one caller list. A genuinely EMPTY string ("") is ignored
// (composeAdCopy pads the list to the minimum); a WHITESPACE-only entry is NOT — it is a
// non-empty caller value that carries no word, which the CampaignInput contract rejects, so
// it fails the hasWord check below rather than being silently dropped. Every other non-empty
// entry must contain at least one word, no newline, be within its width-aware rune cap, and
// be case-insensitively unique. Checks apply to the trimmed value the ad will actually carry.
//
// The over-length REJECTION below (rather than a silent truncate) is also what keeps two
// distinct caller entries from colliding only after truncation: a caller entry that survives
// this check is already within its width cap, so boundedUniqueCopy never truncates it and
// thus never collapses two distinct caller values into one placeholder-padded slot. Only the
// auto-composed fallback candidates (which are not caller copy and ARE allowed to truncate)
// can be dropped by that dedup, which is by design — they exist to pad to the minimum.
func checkAdCopyList(kind string, items []string, maxCount, singleLimit, wideLimit int) error {
	// Count only NON-EMPTY entries against the maximum: a genuinely empty "" is ignored and
	// padded by composeAdCopy, so it emits no asset and must not count. Otherwise 15 valid
	// headlines plus one "" would be rejected as 16 even though only 15 assets are produced. A
	// whitespace-only entry is non-empty (it fails the word check below) and DOES count.
	nonEmpty := 0
	for _, raw := range items {
		if raw != "" {
			nonEmpty++
		}
	}
	if nonEmpty > maxCount {
		return fmt.Errorf("at most %d %ss are allowed, got %d", maxCount, kind, nonEmpty)
	}
	seen := make([]string, 0, len(items))
	for i, raw := range items {
		// Only a genuinely empty string is skippable; a whitespace-only raw value is a
		// caller-supplied entry with no word and must be rejected (per the contract), so it
		// falls through to the hasWord check rather than being silently discarded.
		if raw == "" {
			continue
		}
		s := strings.TrimSpace(raw)
		// Reject ANY control rune, not just \n/\r: a \t, \v, \f, or NUL embedded in caller
		// copy would otherwise reach POST /Ads verbatim and be rejected by Microsoft only
		// AFTER the campaign/ad group exist, orphaning the PAUSED shell — the same post-create
		// failure the newline check guards against. Mirrors sanitizeNamePart, which maps all
		// control runes. Checks raw (pre-trim) so a leading/trailing control char is caught too.
		if idx := strings.IndexFunc(raw, unicode.IsControl); idx >= 0 {
			return fmt.Errorf("%s %d must not contain a control character", kind, i+1)
		}
		if !hasWord(s) {
			return fmt.Errorf("%s %d must contain at least one word", kind, i+1)
		}
		if limit := adCopyLimit(s, singleLimit, wideLimit); utf8.RuneCountInString(s) > limit {
			// Name WHY the cap is the reduced one when a wide character forced it: otherwise a
			// caller sees "exceeds 15 characters" on what looks like a 20-char headline and can't
			// tell that a CJK/emoji glyph halved the limit. The wide cap is Microsoft's, not ours.
			if hasDoubleWidth(s) {
				return fmt.Errorf("%s %d exceeds %d characters (the reduced limit applies because it contains a double-width character)", kind, i+1, limit)
			}
			return fmt.Errorf("%s %d exceeds %d characters", kind, i+1, limit)
		}
		// EqualFold (not a ToLower map key) so Unicode case variants like Σ/ς
		// are caught as duplicates; the list is capped at maxCount so the
		// linear scan is bounded.
		for _, k := range seen {
			if strings.EqualFold(k, s) {
				return fmt.Errorf("%s %d is a duplicate (case-insensitive): %q", kind, i+1, s)
			}
		}
		seen = append(seen, s)
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

// canonicalFinalURL reduces a final URL to a representation-independent key so two URLs
// that differ ONLY in encoding compare equal. It is the idempotency key comparator for
// findAdByFinalURL: buildAdFinalURL emits one canonical spelling, but Microsoft may hand
// back a differently-but-equivalently encoded FinalUrls value. Folds away:
//   - scheme + host case (schemes/hosts are case-insensitive),
//   - a redundant default port (:80 for http, :443 for https),
//   - query-parameter ordering and percent-escape spelling (re-decoded, then re-encoded
//     with sorted keys via url.Values.Encode — the same normal form buildAdFinalURL uses).
//
// It deliberately does NOT change the path's meaning (paths are case-sensitive and a
// trailing slash can be significant) NOR the #fragment: Microsoft preserves the fragment in
// a stored FinalUrls value, so two URLs differing only in fragment are DIFFERENT
// destinations and must not collapse to the same idempotency key. The one path change it
// DOES make is folding the CASE of percent-escape hex digits (`%2f` -> `%2F`) — a
// representation-only difference per RFC 3986 that carries no meaning — WITHOUT decoding the
// escapes (so `%2F` never becomes `/`, which would change the path). It only ever folds
// differences that carry no meaning. If the URL cannot be parsed, the trimmed original is
// returned so an unparseable value still compares byte-for-byte rather than matching all.
func canonicalFinalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	isIPv6 := strings.Contains(host, ":")
	if !isIPv6 {
		// Fold an internationalized domain name to its ASCII (punycode) A-label, so a Unicode
		// host and its `xn--` form — the SAME DNS name — canonicalize equal. This client
		// accepts Unicode/CJK hosts, and Microsoft may store or return the destination in
		// punycode; a lowercase-only host would then miss and stack a duplicate ad. A plain
		// IPv4 address or an already-ASCII host passes through unchanged; if IDNA conversion
		// fails (a malformed label), keep the lowercased host rather than dropping the URL.
		if ascii, ierr := idna.Lookup.ToASCII(host); ierr == nil && ascii != "" {
			host = ascii
		}
	}
	if port := u.Port(); port != "" && !isDefaultPort(u.Scheme, port) {
		// net.JoinHostPort re-adds the [] around an IPv6 literal when a port is present.
		host = net.JoinHostPort(host, port)
	} else if isIPv6 {
		// An IPv6 literal with NO port: Hostname() stripped the brackets, so re-add them.
		// Without this the reassembled URL host is malformed (e.g. ::1 instead of [::1]).
		host = "[" + host + "]"
	}
	u.Host = host
	// Normalize an EMPTY path to "/" for an http(s) URL with a host: `https://h?q` and
	// `https://h/?q` resolve to the same request target, and Microsoft may return either
	// spelling. Without this they'd key differently and a retry could miss the existing ad.
	// Only applies when there's a host (so an opaque/relative URL isn't rewritten) and the
	// path is genuinely empty (a non-empty path, including a bare "/", is left as-is).
	if u.Host != "" && u.EscapedPath() == "" && (u.Scheme == "http" || u.Scheme == "https") {
		u.Path = "/"
		u.RawPath = ""
	}
	// Normalize percent-escapes in the path so two spellings of the SAME request target key
	// equal: (1) DECODE escapes of RFC 3986 unreserved bytes (A-Z a-z 0-9 - . _ ~) to their
	// literal form — `/a%7Eb` and `/a~b` are the same target, and Microsoft may return either,
	// so without this a retry would miss the existing ad and post a duplicate; (2) upper-case
	// the hex of every REMAINING (reserved/other) escape so `%2f` and `%2F` match, WITHOUT
	// decoding it — an escaped slash `%2F` must stay distinct from a literal `/`, which carries
	// a different meaning. Set RawPath so u.String() emits this exact normalized spelling.
	escaped := u.EscapedPath()
	u.RawPath = upperPercentEscapes(decodeUnreservedEscapes(escaped))
	// Re-decode then re-encode the query into url.Values' normal form (sorted keys,
	// canonical escaping) so param order and %-escape casing don't affect the key. The
	// #fragment is left intact (see the doc above) — it is part of the destination.
	u.RawQuery = u.Query().Encode()
	return u.String()
}

// decodeUnreservedEscapes decodes every percent-escape whose octet is an RFC 3986 UNRESERVED
// byte (A-Z a-z 0-9 - . _ ~) to that literal character, leaving all other escapes (reserved
// or non-ASCII, e.g. `%2F`, `%20`, `%E4`) untouched. Per RFC 3986 §2.3 an unreserved octet
// and its percent-encoding are equivalent, so decoding them never changes the target — it
// only canonicalizes spelling (`%7E` -> `~`). A malformed escape (fewer than two following
// hex digits) is left as-is. Used to key `/a%7Eb` and `/a~b` — the same target — equal.
func decodeUnreservedEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			if oct := hexByte(s[i+1], s[i+2]); isUnreservedByte(oct) {
				b.WriteByte(oct)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isUnreservedByte reports whether c is an RFC 3986 §2.3 unreserved character.
func isUnreservedByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
		c == '-', c == '.', c == '_', c == '~':
		return true
	}
	return false
}

// hexByte returns the byte value of the two-hex-digit pair (hi, lo). Callers must ensure both
// are hex digits (isHexDigit).
func hexByte(hi, lo byte) byte {
	return hexVal(hi)<<4 | hexVal(lo)
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default: // 'A'-'F'
		return c - 'A' + 10
	}
}

// upperPercentEscapes upper-cases the two hex digits following each '%' in s, leaving every
// other byte (including the escaped octet's meaning) unchanged. `%2f` -> `%2F`. A malformed
// escape (fewer than two following hex digits) is left as-is. This normalizes escape SPELLING
// only — it never decodes, so it can't change a path's meaning.
func upperPercentEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' && isHexDigit(b[i+1]) && isHexDigit(b[i+2]) {
			b[i+1] = upperHex(b[i+1])
			b[i+2] = upperHex(b[i+2])
			i += 2
		}
	}
	return string(b)
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func upperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - ('a' - 'A')
	}
	return c
}

// isDefaultPort reports whether port is the scheme's default (and thus omittable without
// changing the destination): 80 for http, 443 for https.
func isDefaultPort(scheme, port string) bool {
	// Scheme names are case-insensitive (RFC 3986); lower-case so an HTTPS://…:443 URL is
	// recognized as a default port by every caller, not only the ones that pre-lower-case.
	switch strings.ToLower(scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	default:
		return false
	}
}

// authorityForWidth returns the authority in the SAME form the ad's final URL carries, which is
// what the display-domain width check must measure. host is the (possibly IDNA-decoded) hostname
// with brackets already stripped by url.Hostname().
//
// Both bracket cases matter: JoinHostPort re-adds them when there is a port, and the second
// branch re-adds them when there is not — Hostname() having removed them would otherwise leave
// the count two runes short of the string canonicalFinalURL emits, so a host at the limit could
// pass the check and be rejected upstream, orphaning a PAUSED campaign and ad group.
func authorityForWidth(u *url.URL, host string) string {
	// Lower-case the scheme before the default-port test: validateAdURL accepts any scheme
	// casing and buildAdFinalURL preserves it, so a valid HTTPS://…:443 would otherwise miss
	// the case-sensitive "https" match and wrongly count :443 against the host length.
	if port := u.Port(); port != "" && !isDefaultPort(strings.ToLower(u.Scheme), port) {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
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
		// Bound the echoed scheme: it comes from the unbounded caller URL and this error can
		// be persisted, so a megabyte-scale scheme would otherwise produce a megabyte error.
		return fmt.Errorf("registration URL %q must use an http or https scheme, got %q", redactAdURL(raw), truncate(u.Scheme, maxErrorBodyChars))
	}
}

// redactAdURL returns a URL safe to echo in an error: scheme://host/path only (query,
// fragment, and userinfo dropped). Mirrors the reddit/meta redactURL.
func redactAdURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if u, err := url.Parse(trimmed); err == nil && u.IsAbs() && u.Host != "" {
		redacted := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
		// Bound the result: dropping the query/fragment/userinfo still leaves an unbounded
		// caller-controlled scheme/host/path, and this string can be persisted in an error.
		// The fallback path below already caps via truncate; apply the same cap here.
		return truncate(redacted.String(), maxErrorBodyChars)
	}
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if strings.Contains(trimmed, "@") {
		return "[unparseable-url-redacted]"
	}
	return truncate(trimmed, maxErrorBodyChars)
}
