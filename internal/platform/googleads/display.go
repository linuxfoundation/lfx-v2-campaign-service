// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"net/http"
)

const (
	// advertisingChannelDisplay is the channel type for a standard Display campaign —
	// the Google Display Network's banner inventory — as opposed to SEARCH,
	// DEMAND_GEN, PERFORMANCE_MAX or VIDEO.
	advertisingChannelDisplay = "DISPLAY"

	// adGroupTypeDisplayStandard is the ad group type a responsive display ad requires.
	//
	// Sent explicitly, like the Video group's type and unlike the Demand Gen group's
	// omitted one, because the default Google applies to an ad group whose type is
	// absent is not guaranteed to be this one — and a mistyped group rejects the
	// adGroupAds:mutate AFTER the budget, the campaign and the group have all
	// committed. Spelling it is one field; getting it wrong is an orphan.
	adGroupTypeDisplayStandard = "DISPLAY_STANDARD"
)

// displayCampaignCreate is a FIFTH payload shape rather than a widened sibling, for the
// reason its four predecessors each state: the channels disagree on required fields.
//
// Unlike Video, Display carries NO `advertisingChannelSubType`. A bare VIDEO campaign is
// ambiguous between reach, bumper, sequence and action products, which is why that
// channel must pin one; a bare DISPLAY campaign is already the standard Display campaign
// this client creates, and the sub-types that exist there — the smart and gmail variants
// — are products with different ad shapes that this cascade does not build. Omitting the
// field is therefore a positive choice, not an oversight.
type displayCampaignCreate struct {
	Name                           string `json:"name"`
	Status                         string `json:"status"`
	AdvertisingChannelType         string `json:"advertisingChannelType"`
	CampaignBudget                 string `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string `json:"containsEuPoliticalAdvertising"`
	// biddingFields is embedded ANONYMOUSLY, as on all four sibling payloads, so the
	// oneof invariant keeps one definition. Which strategies may be sent is
	// channel-specific and lives in validateBiddingPlan — here, the four portfolio
	// strategies, with the live-verification caveat recorded there. Manual CPC is
	// refused on this channel even though Google accepts it; see
	// displayBiddingStrategies for why.
	biddingFields
	// Display attaches its location criteria at the CAMPAIGN level, like Search,
	// Performance Max and Video and unlike Demand Gen — so this setting governs
	// criteria this campaign actually carries. See the type's doc in campaign.go.
	GeoTargetTypeSetting geoTargetTypeSetting `json:"geoTargetTypeSetting"`
	// StartDateTime/EndDateTime are the same campaign-level v23 fields every other
	// payload carries, resolved by the same shared preflight. Carrying them is a
	// correctness requirement, not a nicety: applyCampaignConfig records the window on
	// the row regardless, so a channel that validated a window and did not send it
	// would make the campaigns table claim a date the campaign does not have.
	StartDateTime string `json:"startDateTime,omitempty"`
	EndDateTime   string `json:"endDateTime,omitempty"`
}

// displayAdGroupCreate is the ad group a responsive display ad hangs off.
//
// It carries no CPC bid, for the reason the Demand Gen and Video groups do not: the
// preflight refuses a manual bid on every non-Search channel. On THIS channel that
// refusal is a client limitation rather than an upstream one — Google does accept manual
// CPC on Display — and displayBiddingStrategies is where that is written down.
type displayAdGroupCreate struct {
	Name     string `json:"name"`
	Campaign string `json:"campaign"`
	Status   string `json:"status"`
	Type     string `json:"type"`
}

// CreateDisplayCampaign creates a PAUSED Display campaign: images → budget → campaign →
// campaign-level geo → campaign-level criteria → ad group → image assets → ad.
//
// Seven mutating steps behind a fetch phase, which makes this the union of the two
// hardest halves in the package: campaign-level criteria like Search, Performance Max
// and Video, an ad group with an ad under it like Search, Demand Gen and Video, AND a
// pre-budget network phase like Demand Gen and Performance Max. Video had the first two
// and no fetch, because a YouTube video is referenced by id; a responsive display ad is
// built from image BYTES, so this channel pays the fetch cost those two do.
//
// NOT LIVE-VERIFIED, on the same terms as Video: the only Google account reachable from
// here is a production LF account and a validateOnly mutate is still a POST to it. What
// that leaves open is named rather than hedged — the field shapes below are read off
// Google's published v23 resources, and the one thing a code reading cannot settle is
// the BIDDING SET, recorded at displayBiddingStrategies with the check that closes it.
//
// The partial-result contract is the siblings', unchanged: past the campaign create
// every failure returns the error ALONGSIDE a non-nil *CampaignResult carrying what
// exists so far, because returning (nil, err) would release the orchestrator's claim on
// a campaign that exists and spends.
func (c *Client) CreateDisplayCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	pf, err := c.preflightCampaignKind(campaignKindDisplay, in)
	if err != nil {
		return nil, err // pre-create: nothing was sent
	}

	// The images are downloaded BEFORE the first mutate, with everything else that can
	// fail for free. A 404, an over-sized file or an image that misses Google's aspect
	// ratio therefore costs nothing — the orphan-avoidance guarantee the pure
	// validators give, extended over the one check that genuinely needs the network.
	// Nothing has been created yet, so this returns (nil, err) like the preflight above
	// and unlike every step after it.
	images, err := c.fetchDisplayImages(ctx, pf.display)
	if err != nil {
		return nil, err // pre-create: nothing was sent
	}

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
	// client's idempotency key — see budgetKindFor for why the Display budget takes a
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
			return namePartial(), fmt.Errorf("google-ads display budget %q already exists (DUPLICATE_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, err)
		case createOutcomeAmbiguous(err):
			return namePartial(), fmt.Errorf("google-ads display budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		default:
			return nil, fmt.Errorf("google-ads display budget creation failed: %w", err)
		}
	}
	budgetResource, budgetID, err := firstResourceName(budgetResp)
	if err != nil {
		return namePartial(), fmt.Errorf("google-ads display budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))

	budgetPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads display creation aborted after budget %s created (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: the campaign, PAUSED like every campaign this client creates.
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: displayCampaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelDisplay,
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
			return budgetPartial(), fmt.Errorf("google-ads display campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads display campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads display campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads display campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if verr := c.validateCampaignResource(campaignResource); verr != nil {
		return budgetPartial(), fmt.Errorf("google-ads display campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, verr)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, %s, %s, %s)", campaignID, advertisingChannelDisplay, pf.bidding.describe(), flightWindowStep(pf.startDateTime, pf.endDateTime)))

	res := budgetPartial()
	res.CampaignID = campaignID
	res.GoogleAdsURL = "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID
	res.Steps = steps

	// Step 3: geo. CAMPAIGN-level here, like Search, Performance Max and Video — so
	// this is the campaign call, not Demand Gen's ad-group one, and proximity targeting
	// works on this channel for the same reason.
	if !pf.geo.empty() {
		geoIDs, geoErr := c.createCampaignGeoTargeting(ctx, campaignResource, campaignID, pf.geo)
		res.GeoCriterionIDs = geoIDs
		if geoErr != nil {
			return res, geoErr
		}
		steps = append(steps, fmt.Sprintf("Geo targeting applied: %d campaign location criteria (%s)", len(geoIDs), geoStep(in, pf.geo)))
		res.Steps = steps
	}

	// Step 4: the remaining campaign criteria — all five kinds, as on Search and Video.
	// See validateCriteriaPlan for why this channel takes the un-narrowed set where
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
		return res, fmt.Errorf("google-ads display creation aborted after campaign %s created (context done before ad group create): %w", campaignID, ctxErr)
	}

	// Step 5: the ad group, typed DISPLAY_STANDARD so the ad below is a shape it
	// accepts. ENABLED, as on Demand Gen and Video — Search is the only channel here
	// that pauses its ad group, and the campaign is PAUSED regardless, so nothing
	// serves either way.
	adGroupName := displayAdGroupName(in)
	adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: displayAdGroupCreate{
		Name:     adGroupName,
		Campaign: campaignResource,
		Status:   "ENABLED",
		Type:     adGroupTypeDisplayStandard,
	}}}}
	// The deterministic ad-group NAME goes on every failure arm below, as on the Demand
	// Gen, Video and Search paths: a partial without it tells a reconciler an ad group
	// may exist but not what to look for.
	res.AdGroupName = adGroupName
	adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
	if err != nil {
		// Classified exactly as every other mutate in this package: a duplicate name
		// and an ambiguous transport failure both mean the ad group MAY exist, so
		// neither may be reported as a clean failure — a flat "failed" invites a
		// duplicate.
		switch {
		case isDuplicateAdGroupNameErr(err):
			return res, fmt.Errorf("google-ads display ad group %q already exists (DUPLICATE_ADGROUP_NAME) — a prior attempt likely created it; verify in Google Ads before retrying (campaign %s created): %w", adGroupName, campaignID, err)
		case createOutcomeAmbiguous(err):
			return res, fmt.Errorf("google-ads display ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err)
		default:
			return res, fmt.Errorf("google-ads display ad group creation failed (campaign %s created): %w", campaignID, err)
		}
	}
	adGroupResource, adGroupID, err := firstResourceName(adGroupResp)
	if err != nil {
		return res, fmt.Errorf("google-ads display ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err)
	}
	// firstResourceName only extracts a trailing id; it does not check resource kind or
	// account. Without this, a malformed or wrong-account 2xx would be accepted as
	// confirmed and its id persisted as AdGroupID.
	if verr := c.validateResourceKind("adGroups", adGroupResource, true); verr != nil {
		return res, fmt.Errorf("google-ads display ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, verr)
	}
	res.AdGroupID = adGroupID
	steps = append(steps, fmt.Sprintf("Ad group created: %s (%s)", adGroupID, adGroupTypeDisplayStandard))
	res.Steps = steps

	// Steps 6 and 7: the image assets, then the ad that references them — two mutates,
	// because Google has no call that does both. The asset ids are carried on the
	// result even when the ad half failed, so an operator can find assets created with
	// nothing referencing them.
	if pf.display.present {
		assetIDs, adID, adErr := c.createDisplayAd(ctx, adGroupResource, adGroupID, pf.finalURL, pf.display, images)
		res.CreativeAssetIDs = assetIDs
		if adErr != nil {
			return res, adErr
		}
		res.AdID = adID
		steps = append(steps, fmt.Sprintf("Display ad created: %s (PAUSED, %s)", adID, displayCreativeStep(pf.display)))
		res.Steps = steps
	}

	steps = append(steps, displayClosingStep(len(res.GeoCriterionIDs) > 0, res.AdID != ""))
	res.Steps = steps
	return res, nil
}

// displayClosingStep states what still has to be done by hand. Both halves are
// conditional for the reason its four siblings give: an operator who reads "campaign
// created" and believes it is ready will un-pause a campaign that cannot serve, and a
// Display campaign with no ad is exactly that campaign.
func displayClosingStep(hasGeo, hasAd bool) string {
	switch {
	case hasGeo && hasAd:
		return "Display campaign created — review the ad and launch in the Google Ads UI"
	case hasAd:
		return "Display campaign created (no geo targeting set) — add targeting, then review the ad and launch in the Google Ads UI"
	case hasGeo:
		return "Display campaign created with NO AD — it cannot serve until one is added; add a responsive display ad in the Google Ads UI"
	default:
		return "Display campaign created with NO AD (no geo targeting set) — it cannot serve until one is added; add targeting and a responsive display ad in the Google Ads UI"
	}
}
