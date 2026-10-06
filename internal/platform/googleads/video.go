// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"net/http"
)

const (
	// advertisingChannelVideo is the channel type for Video campaigns — YouTube
	// in-stream, in-feed and Shorts inventory — as opposed to SEARCH, DEMAND_GEN or
	// PERFORMANCE_MAX.
	advertisingChannelVideo = "VIDEO"

	// advertisingChannelSubTypeVideoAction pins the campaign to a VIDEO ACTION
	// campaign, and sending it is REQUIRED rather than optional.
	//
	// VIDEO is the only one of this client's four channels whose channel type does
	// not by itself describe a campaign: a bare VIDEO campaign can be a reach, a
	// bumper, a sequence or an action campaign, and they disagree about the ad group
	// type, the ad shape and the bidding strategies they accept. This client creates
	// exactly one of them — the action campaign — because that is the one built
	// around a conversion on a landing page, which is what every campaign this
	// service creates exists to do: the brief carries ONE registration URL with the
	// UTM attribution the data pipeline parses back out, and a reach campaign has
	// nowhere to put it.
	//
	// Pinning it here rather than offering the sub-type as an input is the same
	// judgement budgetKindFor makes about the budget name: a field that changes which
	// ad shapes are legal is not a knob to expose on a path that validates the ad
	// shape before the budget mutate.
	advertisingChannelSubTypeVideoAction = "VIDEO_ACTION"

	// adGroupTypeVideoResponsive is the ad group type a responsive video ad requires.
	//
	// Unlike demandGenAdGroupCreate, which deliberately OMITS the type field, this one
	// must send it: Google defaults a video ad group to VIDEO_TRUE_VIEW_IN_STREAM, and
	// an adGroupAds:mutate carrying a videoResponsiveAd into that group is rejected —
	// after the budget, the campaign and the ad group have all committed.
	adGroupTypeVideoResponsive = "VIDEO_RESPONSIVE"
)

// videoCampaignCreate is a FOURTH payload shape rather than a widened sibling, for
// the reason demandGenCampaignCreate and performanceMaxCampaignCreate both state: the
// channels disagree on required fields. Video is the only one carrying
// `advertisingChannelSubType`, and like Performance Max it rejects `networkSettings`
// and `manualCpc`.
type videoCampaignCreate struct {
	Name                   string `json:"name"`
	Status                 string `json:"status"`
	AdvertisingChannelType string `json:"advertisingChannelType"`
	// AdvertisingChannelSubType is never omitempty: see the constant above for why an
	// unqualified VIDEO campaign is not a campaign this client knows how to fill.
	AdvertisingChannelSubType      string `json:"advertisingChannelSubType"`
	CampaignBudget                 string `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string `json:"containsEuPoliticalAdvertising"`
	// biddingFields is embedded ANONYMOUSLY, as on all three sibling payloads, so the
	// oneof invariant keeps one definition. Which strategies may be sent is
	// channel-specific and lives in validateBiddingPlan — here, the two CPA-side
	// conversion strategies, with the live-verification caveat recorded there.
	biddingFields
	// Video attaches its location criteria at the CAMPAIGN level, like Search and
	// Performance Max and unlike Demand Gen — so this setting governs criteria this
	// campaign actually carries. See the type's doc in campaign.go.
	GeoTargetTypeSetting geoTargetTypeSetting `json:"geoTargetTypeSetting"`
	// StartDateTime/EndDateTime are the same campaign-level v23 fields every other
	// payload carries, resolved by the same shared preflight. Carrying them is a
	// correctness requirement, not a nicety: applyCampaignConfig records the window on
	// the row regardless, so a channel that validated a window and did not send it
	// would make the campaigns table claim a date the campaign does not have.
	StartDateTime string `json:"startDateTime,omitempty"`
	EndDateTime   string `json:"endDateTime,omitempty"`
}

// videoAdGroupCreate is the ad group a responsive video ad hangs off.
//
// It carries an explicit Type where demandGenAdGroupCreate deliberately omits one —
// see adGroupTypeVideoResponsive. It carries no CPC bid for the same reason the
// Demand Gen group does not: the preflight refuses a bid on every non-Search channel,
// because none of them bids manually.
type videoAdGroupCreate struct {
	Name     string `json:"name"`
	Campaign string `json:"campaign"`
	Status   string `json:"status"`
	Type     string `json:"type"`
}

// CreateVideoCampaign creates a PAUSED Video (YouTube) campaign: budget → campaign →
// campaign-level geo → campaign-level criteria → ad group → video assets → ad.
//
// Seven steps, the longest cascade in this package, because Video is the only channel
// that has BOTH campaign-level criteria (like Search and Performance Max) AND an ad
// group with an ad under it (like Search and Demand Gen). Performance Max has the
// first half and an asset group; Demand Gen has the second half with its geo on the
// ad group. This channel is the union.
//
// NOT LIVE-VERIFIED. Every other channel in this package was settled against a
// validateOnly campaigns:mutate on a real account before it shipped; this one has
// not been, because the only Google account reachable from here is a production LF
// account and a validateOnly mutate is still a POST to it. What that leaves open is
// named precisely rather than hedged: the field shapes below are read off Google's
// published v23 resources, and the one thing a code reading cannot settle is the
// BIDDING SET — see videoBiddingStrategies, which records the same caveat and the
// check that closes it. Everything else in this cascade fails, if it fails, the way
// the other three do: with a partial result and an UNCONFIRMED classification.
//
// The partial-result contract is the siblings', unchanged: past the campaign create
// every failure returns the error ALONGSIDE a non-nil *CampaignResult carrying what
// exists so far, because returning (nil, err) would release the orchestrator's claim
// on a campaign that exists and spends.
func (c *Client) CreateVideoCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	pf, err := c.preflightCampaignKind(campaignKindVideo, in)
	if err != nil {
		return nil, err // pre-create: nothing was sent
	}

	// There is no pre-budget fetch phase here, and its absence is the channel rather
	// than a missing guard: this creative's only binary asset is a YouTube video,
	// which lives on YouTube and is referenced by id. See the file comment in
	// video_creative.go.

	campaignName := pf.campaignName
	budgetName := pf.budgetName
	steps := []string{}

	namePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:           "google-ads",
			AccountLabel:       c.account.Label,
			CustomerID:         c.account.CustomerID,
			CampaignName:       campaignName,
			CampaignBudgetName: budgetName,
			Steps:              steps,
		}
	}

	// Step 1: the budget. Channel-agnostic, and non-shared so its name stays this
	// client's idempotency key — see budgetKindFor for why the Video budget takes a
	// channel-specific name segment and Search's does not.
	shared := false
	budgetReq := mutateRequest{Operations: []mutateOperation{{Create: campaignBudgetCreate{
		Name:             budgetName,
		AmountMicros:     pf.amountMicros,
		DeliveryMethod:   "STANDARD",
		ExplicitlyShared: &shared,
	}}}}
	budgetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignBudgets:mutate"), budgetReq, false)
	if err != nil {
		switch {
		case isDuplicateBudgetNameErr(err):
			return namePartial(), fmt.Errorf("google-ads video budget %q already exists (DUPLICATE_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, err)
		case createOutcomeAmbiguous(err):
			return namePartial(), fmt.Errorf("google-ads video budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		default:
			return nil, fmt.Errorf("google-ads video budget creation failed: %w", err)
		}
	}
	budgetResource, budgetID, err := firstResourceName(budgetResp)
	if err != nil {
		return namePartial(), fmt.Errorf("google-ads video budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))

	budgetPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads video creation aborted after budget %s created (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: the campaign, PAUSED like every campaign this client creates.
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: videoCampaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelVideo,
		AdvertisingChannelSubType:      advertisingChannelSubTypeVideoAction,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		GeoTargetTypeSetting:           geoTargetTypeSetting{PositiveGeoTargetType: geoTargetPresence},
		StartDateTime:                  pf.startDateTime,
		EndDateTime:                    pf.endDateTime,
		biddingFields:                  pf.bidding.fields(),
	}}}}
	campaignResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads video campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads video campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads video campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads video campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if verr := c.validateCampaignResource(campaignResource); verr != nil {
		return budgetPartial(), fmt.Errorf("google-ads video campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, verr)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, VIDEO/%s, %s, %s)", campaignID, advertisingChannelSubTypeVideoAction, pf.bidding.describe(), flightWindowStep(pf.startDateTime, pf.endDateTime)))

	res := budgetPartial()
	res.CampaignID = campaignID
	res.GoogleAdsURL = "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID
	res.Steps = steps

	// Step 3: geo. CAMPAIGN-level here, like Search and Performance Max — so this is
	// the campaign call, not Demand Gen's ad-group one, and proximity targeting works
	// on this channel for the same reason.
	if !pf.geo.empty() {
		geoIDs, geoErr := c.createCampaignGeoTargeting(ctx, campaignResource, campaignID, pf.geo)
		res.GeoCriterionIDs = geoIDs
		if geoErr != nil {
			return res, geoErr
		}
		steps = append(steps, fmt.Sprintf("Geo targeting applied: %d campaign location criteria (%s)", len(geoIDs), geoStep(in, pf.geo)))
		res.Steps = steps
	}

	// Step 4: the remaining campaign criteria — all five kinds, as on Search. See
	// validateCriteriaPlan for why this channel takes the un-narrowed set where
	// Demand Gen takes none and Performance Max takes half.
	if !pf.criteria.empty() {
		critIDs, critErr := c.createCampaignTargetingCriteria(ctx, campaignResource, campaignID, pf.criteria)
		res.TargetingCriterionIDs = critIDs
		if critErr != nil {
			return res, critErr
		}
		steps = append(steps, fmt.Sprintf("Campaign targeting applied: %d criteria (%s)", len(critIDs), criteriaStep(pf.criteria)))
		res.Steps = steps
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("google-ads video creation aborted after campaign %s created (context done before ad group create): %w", campaignID, ctxErr)
	}

	// Step 5: the ad group, typed VIDEO_RESPONSIVE so the ad below is a shape it
	// accepts.
	adGroupName := videoAdGroupName(in)
	adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: videoAdGroupCreate{
		Name:     adGroupName,
		Campaign: campaignResource,
		Status:   "ENABLED",
		Type:     adGroupTypeVideoResponsive,
	}}}}
	// The deterministic ad-group NAME goes on every failure arm below, as on the
	// Demand Gen and Search paths: a partial without it tells a reconciler an ad group
	// may exist but not what to look for.
	res.AdGroupName = adGroupName
	adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
	if err != nil {
		// Classified exactly as every other mutate in this package: a duplicate name and
		// an ambiguous transport failure both mean the ad group MAY exist, so neither may
		// be reported as a clean failure — a flat "failed" invites a duplicate.
		switch {
		case isDuplicateAdGroupNameErr(err):
			return res, fmt.Errorf("google-ads video ad group %q already exists (DUPLICATE_ADGROUP_NAME) — a prior attempt likely created it; verify in Google Ads before retrying (campaign %s created): %w", adGroupName, campaignID, err)
		case createOutcomeAmbiguous(err):
			return res, fmt.Errorf("google-ads video ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err)
		default:
			return res, fmt.Errorf("google-ads video ad group creation failed (campaign %s created): %w", campaignID, err)
		}
	}
	adGroupResource, adGroupID, err := firstResourceName(adGroupResp)
	if err != nil {
		return res, fmt.Errorf("google-ads video ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err)
	}
	// firstResourceName only extracts a trailing id; it does not check resource kind or
	// account. Without this, a malformed or wrong-account 2xx would be accepted as
	// confirmed and its id persisted as AdGroupID.
	if verr := c.validateResourceKind("adGroups", adGroupResource, true); verr != nil {
		return res, fmt.Errorf("google-ads video ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, verr)
	}
	res.AdGroupID = adGroupID
	steps = append(steps, fmt.Sprintf("Ad group created: %s (%s)", adGroupID, adGroupTypeVideoResponsive))
	res.Steps = steps

	// Steps 6 and 7: the video assets, then the ad that references them — two mutates,
	// because Google has no call that does both. The asset ids are carried on the
	// result even when the ad half failed, so an operator can find assets created with
	// nothing referencing them.
	if pf.video.present {
		assetIDs, adID, adErr := c.createVideoAd(ctx, adGroupResource, adGroupID, pf.finalURL, pf.video)
		res.CreativeAssetIDs = assetIDs
		if adErr != nil {
			return res, adErr
		}
		res.AdID = adID
		steps = append(steps, fmt.Sprintf("Video ad created: %s (PAUSED, %s)", adID, videoCreativeStep(pf.video)))
		res.Steps = steps
	}

	steps = append(steps, videoClosingStep(len(res.GeoCriterionIDs) > 0, res.AdID != ""))
	res.Steps = steps
	return res, nil
}

// videoClosingStep states what still has to be done by hand. Both halves are
// conditional for the reason its three siblings give: an operator who reads "campaign
// created" and believes it is ready will un-pause a campaign that cannot serve, and a
// Video campaign with no ad is exactly that campaign.
func videoClosingStep(hasGeo, hasAd bool) string {
	switch {
	case hasGeo && hasAd:
		return "Video campaign created — review the ad and launch in the Google Ads UI"
	case hasAd:
		return "Video campaign created (no geo targeting set) — add targeting, then review the ad and launch in the Google Ads UI"
	case hasGeo:
		return "Video campaign created with NO AD — it cannot serve until one is added; add a responsive video ad in the Google Ads UI"
	default:
		return "Video campaign created with NO AD (no geo targeting set) — it cannot serve until one is added; add targeting and a responsive video ad in the Google Ads UI"
	}
}
