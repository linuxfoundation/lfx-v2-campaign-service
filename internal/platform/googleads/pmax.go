// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"net/http"
)

const (
	// advertisingChannelPerformanceMax is the channel type for Performance Max —
	// one campaign serving Search, Display, YouTube, Discover, Gmail and Maps from a
	// single asset group, as opposed to SEARCH or DEMAND_GEN.
	advertisingChannelPerformanceMax = "PERFORMANCE_MAX"
)

// performanceMaxCampaignCreate is a THIRD payload shape rather than a widened
// campaignCreate, for the reason demandGenCampaignCreate states: the channels disagree
// on required fields. Performance Max rejects `networkSettings` (it has no network to
// choose — it serves on all of them by definition) and rejects `manualCpc`, and it is
// the only one of the three that carries `urlExpansionOptOut`.
type performanceMaxCampaignCreate struct {
	Name                           string `json:"name"`
	Status                         string `json:"status"`
	AdvertisingChannelType         string `json:"advertisingChannelType"`
	CampaignBudget                 string `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string `json:"containsEuPoliticalAdvertising"`
	// biddingFields is embedded ANONYMOUSLY, as on both other channel payloads, so the
	// oneof invariant keeps one definition. What is channel-specific is which
	// strategies may be sent, and that lives in validateBiddingPlan — here, the four
	// conversion-based strategies, because Performance Max has no manual bidding and
	// no maximize-clicks.
	biddingFields
	// Performance Max attaches its location criteria at the CAMPAIGN level, like
	// Search and unlike Demand Gen — so this setting governs criteria this campaign
	// actually carries, and without it a "US" target serves to anyone worldwide
	// showing interest in the US. See the type's doc in campaign.go.
	GeoTargetTypeSetting geoTargetTypeSetting `json:"geoTargetTypeSetting"`
	// UrlExpansionOptOut is sent EXPLICITLY as true rather than omitted, and that is a
	// deliberate choice rather than a default being restated.
	//
	// Left opted IN — Google's own default — Performance Max generates its own landing
	// pages by crawling the advertiser's whole site and sends traffic wherever its model
	// prefers. Every campaign this service creates is for ONE event with ONE
	// registration URL, composed with the UTM attribution the data pipeline parses back
	// out; a campaign free to substitute its own final URL spends the event's budget on
	// pages that carry none of that, and the spend then reconciles against nothing.
	//
	// An operator who wants the expansion can turn it off in the Google Ads UI. The
	// reverse — discovering after the fact that attribution was silently bypassed — is
	// not recoverable, which is what makes this the safe side of the default.
	UrlExpansionOptOut bool `json:"urlExpansionOptOut"`
	// StartDateTime/EndDateTime are the same campaign-level v23 fields both other
	// payloads carry, resolved by the same shared preflight. See demandgen.go for why
	// carrying them is a correctness requirement and not a nicety: applyCampaignConfig
	// records the window on the row regardless, so a channel that validated a window
	// and did not send it would make the campaigns table claim a date the campaign
	// does not have.
	StartDateTime string `json:"startDateTime,omitempty"`
	EndDateTime   string `json:"endDateTime,omitempty"`
}

// CreatePerformanceMaxCampaign creates a PAUSED Performance Max campaign: budget →
// campaign → campaign-level geo → campaign-level criteria → asset group.
//
// It creates NO AD GROUP AND NO AD, and that is the channel rather than a gap: a
// Performance Max campaign has neither. Its creative is an ASSET GROUP, built by the
// three mutates in pmax_creative.go, and the preflight refuses ad groups, keywords and
// a CPC bid rather than dropping them.
//
// The partial-result contract is CreateCampaign's and CreateDemandGenCampaign's,
// unchanged: past the campaign create every failure returns the error ALONGSIDE a
// non-nil *CampaignResult carrying what exists so far, because returning (nil, err)
// would release the orchestrator's claim on a campaign that exists and spends.
func (c *Client) CreatePerformanceMaxCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	pf, err := c.preflightCampaignKind(campaignKindPerformanceMax, in)
	if err != nil {
		return nil, err // pre-create: nothing was sent
	}

	// The asset-group images are downloaded BEFORE the first mutate, exactly as the
	// Demand Gen cascade downloads its creative's, and through the same hardened
	// fetcher. A 404 or an image that misses Google's minimum therefore costs nothing.
	images, err := c.fetchSlotImages(ctx, performanceMaxImageSlots, pf.pmax.urls)
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
	// client's idempotency key — see budgetKindFor for why the Performance Max budget
	// takes a channel-specific name segment and Search's does not.
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
			return namePartial(), fmt.Errorf("google-ads performance max budget %q already exists (DUPLICATE_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, err)
		case createOutcomeAmbiguous(err):
			return namePartial(), fmt.Errorf("google-ads performance max budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		default:
			return nil, fmt.Errorf("google-ads performance max budget creation failed: %w", err)
		}
	}
	budgetResource, budgetID, err := firstResourceName(budgetResp)
	if err != nil {
		return namePartial(), fmt.Errorf("google-ads performance max budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))

	budgetPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads performance max creation aborted after budget %s created (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: the campaign, PAUSED like every campaign this client creates.
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: performanceMaxCampaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelPerformanceMax,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		GeoTargetTypeSetting:           geoTargetTypeSetting{PositiveGeoTargetType: geoTargetPresence},
		UrlExpansionOptOut:             true,
		StartDateTime:                  pf.startDateTime,
		EndDateTime:                    pf.endDateTime,
		// Resolved by the SHARED preflight, which on this channel admits only the four
		// conversion-based strategies — Performance Max has no manual bidding at all.
		biddingFields: pf.bidding.fields(),
	}}}}
	campaignResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads performance max campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if verr := c.validateCampaignResource(campaignResource); verr != nil {
		return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, verr)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, PERFORMANCE_MAX, %s, %s)", campaignID, pf.bidding.describe(), flightWindowStep(pf.startDateTime, pf.endDateTime)))

	res := budgetPartial()
	res.CampaignID = campaignID
	res.GoogleAdsURL = "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID
	res.Steps = steps

	// Step 3: geo. CAMPAIGN-level here, like Search — so this is the Search call, not
	// Demand Gen's ad-group one, and proximity targeting works on this channel for the
	// same reason.
	if !pf.geo.empty() {
		geoIDs, geoErr := c.createCampaignGeoTargeting(ctx, campaignResource, campaignID, pf.geo)
		res.GeoCriterionIDs = geoIDs
		if geoErr != nil {
			return res, geoErr
		}
		steps = append(steps, fmt.Sprintf("Geo targeting applied: %d campaign location criteria (%s)", len(geoIDs), geoStep(in, pf.geo)))
		res.Steps = steps
	}

	// Step 4: the remaining campaign criteria. On this channel the preflight has
	// already narrowed these to languages and ad schedules — device bid modifiers and
	// campaign-level demographic exclusions are refused, because Performance Max does
	// not accept them.
	if !pf.criteria.empty() {
		critIDs, critErr := c.createCampaignTargetingCriteria(ctx, campaignResource, campaignID, pf.criteria)
		res.TargetingCriterionIDs = critIDs
		if critErr != nil {
			return res, critErr
		}
		steps = append(steps, fmt.Sprintf("Campaign targeting applied: %d criteria (%s)", len(critIDs), criteriaStep(pf.criteria)))
		res.Steps = steps
	}

	// Step 5: the asset group — assets, the group, then the links. Like the Demand Gen
	// creative step, the asset ids are carried on the result even when a later half
	// failed, so an operator can find assets created with nothing referencing them.
	if pf.pmax.present {
		assetIDs, groupID, links, agErr := c.createPerformanceMaxAssetGroup(ctx, campaignResource, campaignID, pf.finalURL, pf.pmax, images)
		res.CreativeAssetIDs = assetIDs
		res.AssetGroupID = groupID
		// Recorded on BOTH paths, and recorded even when it is zero. The group id is kept on
		// a failure so an operator can find an empty asset group; the count is what stops the
		// activation gate from reading that same kept id as "ready to serve". On the error
		// path createPerformanceMaxAssetGroup returns 0 both when the link mutate definitely
		// failed and when its outcome is unconfirmed — see the field's doc for why zero is
		// the right record for the ambiguous case too.
		res.AssetGroupAssetLinks = &links
		if agErr != nil {
			return res, agErr
		}
		steps = append(steps, fmt.Sprintf("Asset group created: %s (PAUSED, %d asset(s) linked, %s)", groupID, links, performanceMaxCreativeStep(pf.pmax)))
		res.Steps = steps
	}

	steps = append(steps, performanceMaxClosingStep(len(res.GeoCriterionIDs) > 0, res.AssetGroupID != ""))
	res.Steps = steps
	return res, nil
}

// performanceMaxClosingStep states what still has to be done by hand. Both halves are
// conditional for the reason demandGenClosingStep gives: an operator who reads
// "campaign created" and believes it is ready will un-pause a campaign that cannot
// serve. A Performance Max campaign with no asset group is exactly that campaign.
func performanceMaxClosingStep(hasGeo, hasAssetGroup bool) string {
	switch {
	case hasGeo && hasAssetGroup:
		return "Performance Max campaign created — review the asset group and launch in the Google Ads UI"
	case hasAssetGroup:
		return "Performance Max campaign created (no geo targeting set) — add targeting, then review the asset group and launch in the Google Ads UI"
	case hasGeo:
		return "Performance Max campaign created with NO ASSET GROUP — it cannot serve until one is added; build the asset group in the Google Ads UI"
	default:
		return "Performance Max campaign created with NO ASSET GROUP (no geo targeting set) — it cannot serve until one is added; add targeting and build the asset group in the Google Ads UI"
	}
}
