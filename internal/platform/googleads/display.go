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
	// NetworkSettings is REQUIRED here, and Display is the only non-Search channel that
	// takes it. Demand Gen, Performance Max and Video all REJECT the field — their
	// network is implied by the channel and has no knobs — which makes the obvious
	// inference ("Display is not Search, so omit it like the other three") exactly
	// wrong. An omitted networkSettings is not "Google picks a sensible default": the
	// flags are proto3 bools, so absent resolves to a campaign targeting NO network,
	// and Google rejects that with CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK
	// — AFTER the budget mutate has committed. That is the orphan this cascade's whole
	// ordering exists to avoid, and a retry composes the same budget name and dies at
	// DUPLICATE_NAME, so it never reconciles itself.
	//
	// targetContentNetwork is the one true flag: the Google Display Network IS the
	// content network, and targetGoogleSearch/targetSearchNetwork stay false because a
	// Display campaign has no business opting itself into Search inventory. This is what
	// the Google Ads API team tells callers to send for a DISPLAY campaign, rather than
	// something inferred from the Search shape.
	NetworkSettings networkSettings `json:"networkSettings"`
	// biddingFields is embedded ANONYMOUSLY, as on all four sibling payloads, so the
	// oneof invariant keeps one definition. Which strategies may be sent is
	// channel-specific and lives in validateBiddingPlan — here, the four portfolio
	// strategies, with the probe that settled them recorded there. Manual CPC is
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
	// URLCustomParameters is the utmcampaign custom parameter (#476), from the same
	// shared preflight as on every sibling payload; see campaignCreate.URLCustomParameters.
	URLCustomParameters []urlCustomParameter `json:"urlCustomParameters,omitempty"`
}

// displayAdGroupCreate is the ad group a responsive display ad hangs off.
//
// It carries no CPC bid, for the reason the Demand Gen and Video groups do not: the
// preflight refuses a manual bid on every non-Search channel. On THIS channel that
// refusal is a client limitation rather than an upstream one — Google does accept manual
// CPC on Display — and displayBiddingStrategies is where that is written down.
//
// TargetingSetting is sent ONLY when the ad group carries audience criteria (#462), built by
// displayAdGroupTargetingSetting: the four demographic dimensions restated at Google's default
// bidOnly true, then AUDIENCE bidOnly false, so those criteria target rather than observe —
// the opposite of Search's ad group, which sends bidOnly true. Demographic targeting (#499)
// never changes it: Google refuses a demographic dimension set bidOnly false, so that
// targeting narrows through negative criteria instead. With no audience input the field is
// omitted and the payload is byte-identical to the one sent before #462. See
// createDisplayAudienceCriteria for why the setting is sent rather than left to
// Google's default.
type displayAdGroupCreate struct {
	Name     string `json:"name"`
	Campaign string `json:"campaign"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	// TargetingSetting: see the type's doc. A pointer with omitempty so its absence is
	// the pre-#462 payload exactly.
	TargetingSetting *targetingSetting `json:"targetingSetting,omitempty"`
}

// CreateDisplayCampaign creates a PAUSED Display campaign: images → budget → campaign →
// campaign-level geo → campaign-level criteria → ad group → ad-group exclusions →
// ad-group audience criteria → campaign-level placement exclusions → campaign-level
// audience exclusions (#475) → image assets → ad.
//
// With DisplayAdGroups (#475) the ad group, its income/parental exclusions, its audience
// criteria and its own user-list exclusions repeat per group before the campaign-level
// exclusions, and the image assets and ad repeat per group after them. Every criteria
// step runs before any ad, as it did with one group.
//
// Up to eleven mutating steps for one group (plus the conversion goal every channel may
// apply) behind a fetch phase, which makes this the union of the two hardest halves in
// the package: campaign-level criteria like Search, Performance Max and Video, an ad
// group with an ad under it like Search, Demand Gen and Video, AND a pre-budget network
// phase like Demand Gen and Performance Max. Video had the first two and no fetch,
// because a YouTube video is referenced by id; a responsive display ad is built from
// image BYTES, so this channel pays the fetch cost those two do.
//
// NOT LIVE-VERIFIED, on the same terms as Video: the only Google account reachable from
// here is a production LF account and a validateOnly mutate is still a POST to it. What
// that leaves open is named rather than hedged — the field shapes below are read off
// Google's published v23 resources, and the one thing a code reading cannot settle is
// the BIDDING SET, recorded at displayBiddingStrategies with the check that closes it.
// #473's ad-group income-range and parental-status exclusion shapes, and #462's audience
// and placement criterion shapes and the ad group's targetingSetting, were since settled
// by validateOnly probes on 2026-11-03 (see display_targeting.go), and the bidding set by
// one on 2026-11-11 (see displayBiddingStrategies).
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
	// Every ad group's images in one walk (#475); see fetchDisplayGroupImages.
	groupImages, err := c.fetchDisplayGroupImages(ctx, pf.displayGroups)
	if err != nil {
		return nil, imageFetchRefusal(ctx, err) // pre-create: nothing was sent
	}

	campaignName := pf.campaignName
	budgetName := pf.budgetName
	steps := []string{}

	namePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:           platformGoogleAds,
			AccountLabel:       c.account.Label,
			CustomerID:         c.account.CustomerID,
			CampaignName:       campaignName,
			CampaignBudgetName: budgetName,
			Steps:              steps,
		}
	}

	// Before the first write: a campaign of this name already in the account means a retry
	// of a create that got past the campaign step, and the budget create below would succeed
	// and orphan a second budget (#540). See refuseIfCampaignExists.
	if r, err := c.refuseIfCampaignExists(ctx, "display", campaignName, namePartial); err != nil {
		return r, err
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
	// The arms are a single switch rather than `if err != nil { switch }` so the
	// DUPLICATE_NAME arm can resolve to a usable budget instead of only to an error; see
	// reuseBudgetAfterDuplicateName.
	var budgetResource, budgetID string
	switch {
	case err == nil:
		var recovered string
		budgetResource, budgetID, recovered, err = c.budgetFromCreateResponse(ctx, "display", budgetName, pf.amountMicros, budgetResp)
		if err != nil {
			return namePartial(), err
		}
		steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))
		if recovered != "" {
			steps = append(steps, recovered)
		}
	case isDuplicateBudgetNameErr(err):
		resource, id, reuseSteps, reuseErr := c.reuseBudgetAfterDuplicateName(ctx, budgetName, pf.amountMicros, in.Budget)
		if reuseErr != nil {
			// BOTH errors are wrapped, and the DUPLICATE_NAME refusal goes FIRST
			// because that order is load-bearing: isDuplicateBudgetNameErr (and every
			// other errors.As predicate) stops at the FIRST *apiError in the chain, so
			// wrapping the reuse failure ahead of it would let an apiError from the
			// lookup READ shadow the duplicate code and cost this failure its precise
			// create-outcome token. carriesUnconfirmedWrite searches the whole chain,
			// so the reuse side is still read for a write that may have landed.
			return namePartial(), fmt.Errorf("google-ads display budget %q already exists (DUPLICATE_NAME): %w — and could not be safely reused: %w — a prior attempt likely created it; verify in Google Ads before retrying", budgetName, err, reuseErr)
		}
		budgetResource, budgetID = resource, id
		steps = append(steps, reuseSteps...)
	case createOutcomeAmbiguous(err):
		// Resolved against the account rather than reported as unknown — see
		// ambiguousBudgetCreateOutcome; every channel takes this arm identically.
		return c.ambiguousBudgetCreateOutcome(ctx, "display", budgetName, namePartial, err)
	default:
		return nil, fmt.Errorf("google-ads display budget creation failed: %w", err)
	}

	budgetPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads display creation aborted after budget %s was created or reused (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: the campaign, PAUSED like every campaign this client creates.
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: displayCampaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelDisplay,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		NetworkSettings:                networkSettings{TargetContentNetwork: true},
		GeoTargetTypeSetting:           geoTargetTypeSetting{PositiveGeoTargetType: geoTargetPresence},
		StartDateTime:                  pf.startDateTime,
		EndDateTime:                    pf.endDateTime,
		biddingFields:                  pf.bidding.fields(),
		URLCustomParameters:            pf.urlCustomParameters,
	}}}}
	campaignResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads display campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s in place) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads display campaign creation UNCONFIRMED (budget %s in place; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads display campaign creation failed (budget %s in place): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads display campaign creation UNCONFIRMED (budget %s in place; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if verr := c.validateCampaignResource(campaignResource); verr != nil {
		return budgetPartial(), fmt.Errorf("google-ads display campaign creation UNCONFIRMED (budget %s in place; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, verr)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, %s, %s, %s)", campaignID, advertisingChannelDisplay, pf.bidding.describe(), flightWindowStep(pf.startDateTime, pf.endDateTime)))
	if pf.utmCampaignStep != "" {
		steps = append(steps, pf.utmCampaignStep)
	}

	res := budgetPartial()
	res.CampaignID = campaignID
	res.GoogleAdsURL = "https://ads.google.com/aw/campaigns?ocid=" + c.account.CustomerID
	res.Steps = steps

	// The campaign-level conversion goal (#436), first of the post-create steps so a failure
	// leaves as little built on the campaign as possible. Same partial-result contract as geo.
	goalStep, goalErr := c.applyCampaignConversionGoal(ctx, campaignID, pf.bidding)
	if goalErr != nil {
		return res, goalErr
	}
	if goalStep != "" {
		steps = append(steps, goalStep)
		res.Steps = steps
	}

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
	// Demand Gen splits it across two levels and refuses demographics, and Performance Max
	// takes half.
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

	// Once per ad group (#475): the ad group, its income/parental exclusions, its audience
	// criteria and its user-list exclusions. pf.displayGroups holds ONE group when the
	// caller named none, built from the campaign-level fields, and for that group every
	// request, step line and result field is what this cascade produced before the list
	// existed.
	//
	// With a list, each group gets an AdGroups entry appended BEFORE its mutate, as on the
	// Search cascade, so a failure on group k returns groups 1..k-1 and what exists of k; the
	// scalar ad group fields stay the FIRST group's. The ads come in a second pass below, so
	// that every criteria step — the campaign-level exclusions included — still runs before
	// the first ad, the only step that makes the campaign servable.
	multi := len(in.DisplayAdGroups) > 0
	adGroupResources := make([]string, len(pf.displayGroups))
	adGroupIDs := make([]string, len(pf.displayGroups))
	// The deterministic ad-group NAME goes on every failure arm below, as on the Demand
	// Gen, Video and Search paths: a partial without it tells a reconciler an ad group
	// may exist but not what to look for.
	res.AdGroupName = pf.displayGroups[0].name
	for k, g := range pf.displayGroups {
		if k > 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, fmt.Errorf("google-ads display creation aborted after %d of %d ad group(s) were created (campaign %s; context done before ad group %q): %w", k, len(pf.displayGroups), campaignID, g.name, ctxErr)
			}
		}
		tag := displayGroupStep(multi, g.name)
		var entry *AdGroupResult
		if multi {
			res.AdGroups = append(res.AdGroups, AdGroupResult{Name: g.name})
			entry = &res.AdGroups[len(res.AdGroups)-1]
		}

		// The ad group, typed DISPLAY_STANDARD so the ad below is a shape it
		// accepts. PAUSED, as on every channel here (#535). The ad below is PAUSED too, which
		// alone keeps a new campaign dark; the PAUSED ad group is an additional safeguard,
		// so serving outside this service's toggle takes enabling every level in the Google
		// Ads UI, as on Search. Activation goes through
		// GoogleAdsDispatcher.ToggleStatus, whose cascade enables every recorded ad group and
		// ad before the campaign.
		adGroupName := g.name
		adGroupCreateVal := displayAdGroupCreate{
			Name:     adGroupName,
			Campaign: campaignResource,
			Status:   StatusPaused,
			Type:     adGroupTypeDisplayStandard,
		}
		// Audience criteria are attached as TARGETING (#462), which this setting declares;
		// without audience input it stays absent, demographic targeting (#499) or not. Per
		// group: each group with audiences carries it, as the 2026-11-03 validateOnly probe of
		// two groups did. See displayAdGroupCreate.
		adGroupCreateVal.TargetingSetting = displayAdGroupTargetingSetting(g.targeting.audienceCount() > 0)
		adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: adGroupCreateVal}}}
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
			return res, fmt.Errorf("google-ads display ad group creation UNCONFIRMED (%q may "+
				"exist — verify in Google Ads before retrying; campaign %s created): %w",
				adGroupName, campaignID, err)
		}
		// firstResourceName only extracts a trailing id; it does not check resource kind or
		// account. Without this, a malformed or wrong-account 2xx would be accepted as
		// confirmed and its id persisted as AdGroupID.
		if verr := c.validateResourceKind("adGroups", adGroupResource, true); verr != nil {
			return res, fmt.Errorf("google-ads display ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, verr)
		}
		adGroupResources[k], adGroupIDs[k] = adGroupResource, adGroupID
		if k == 0 {
			res.AdGroupID = adGroupID
		}
		if entry != nil {
			entry.ID = adGroupID
		}
		steps = append(steps, fmt.Sprintf("Ad group created: %s (%s)", adGroupID, adGroupTypeDisplayStandard)+tag)
		res.Steps = steps

		// The income-range and parental-status exclusions (#473) go on every ad group, the
		// level the validateOnly probes verified for Display, and before the audiences and
		// the ad so a failure has as little built on it as possible. Same partial-result
		// contract.
		//
		// The demographic targeting (#499) rides in the SAME mutate, so a group never ends up
		// with its exclusions and not its targeting, or the reverse.
		if !pf.adGroupDemographics.empty() || pf.displayDemographics.count() > 0 {
			ids, exErr := c.createAdGroupTargeting(ctx, adGroupResource, adGroupID,
				adGroupCriteriaSet{demographics: pf.adGroupDemographics, targeted: pf.displayDemographics})
			if k == 0 {
				res.AdGroupDemographicCriteriaIDs = ids.demographics
				res.AdGroupDemographicTargetingCriteriaIDs = ids.targeted
			}
			if entry != nil {
				entry.DemographicCriteriaIDs = ids.demographics
				entry.DemographicTargetingCriteriaIDs = ids.targeted
			}
			if exErr != nil {
				return res, exErr
			}
			if n := len(ids.demographics); n > 0 {
				steps = append(steps, fmt.Sprintf("Ad group exclusions applied: %d criteria (%s)",
					n, adGroupDemographicStep(pf.adGroupDemographics))+tag)
			}
			if n := len(ids.targeted); n > 0 {
				steps = append(steps, fmt.Sprintf("Demographic targeting applied: %d ad group criteria (%s)",
					n, displayDemographicTargetingStep(pf.displayDemographics))+tag)
			}
			res.Steps = steps
		}

		// The audience segments, on the ad group they need (#462) — user lists,
		// custom audiences, custom affinities and user interests in one atomic mutate.
		// TARGETING, not Search's observation: see createDisplayAudienceCriteria and
		// displayAdGroupCreate.
		if g.targeting.audienceCount() > 0 {
			audienceIDs, audErr := c.createDisplayAudienceCriteria(ctx, adGroupResource, adGroupID, g.targeting)
			if k == 0 {
				res.AudienceCriteriaIDs = audienceIDs
			}
			if entry != nil {
				entry.AudienceCriteriaIDs = audienceIDs
			}
			if audErr != nil {
				return res, audErr
			}
			steps = append(steps, fmt.Sprintf("Audience targeting applied: %d ad group criteria (%s) — attached as targeting (AUDIENCE bidOnly false), not observation; optimized targeting is left at Google's default and may widen reach", len(audienceIDs), displayAudienceStep(g.targeting))+tag)
			res.Steps = steps
		}

		// This group's own user-list exclusions (#475), only on a named group — the
		// single group's exclusions are campaign-level, below.
		if len(g.targeting.excludedUserLists) > 0 {
			exclIDs, exclErr := c.createAdGroupAudienceExclusions(ctx, adGroupResource, adGroupID, g.targeting.excludedUserLists)
			if entry != nil {
				entry.ExcludedAudienceCriteriaIDs = exclIDs
			}
			if exclErr != nil {
				return res, exclErr
			}
			steps = append(steps, fmt.Sprintf("Audience exclusions applied: %d ad group negative user list criteria", len(exclIDs))+tag)
			res.Steps = steps
		}
	}

	// The placement exclusions, at the CAMPAIGN level so they cover every ad group
	// — one atomic campaignCriteria:mutate of negative placement criteria (#462).
	if len(pf.displayTargeting.excludedPlacements) > 0 {
		placementIDs, plErr := c.createCampaignPlacementExclusions(ctx, campaignResource, campaignID, pf.displayTargeting.excludedPlacements)
		res.ExcludedPlacementCriteriaIDs = placementIDs
		if plErr != nil {
			return res, plErr
		}
		steps = append(steps, fmt.Sprintf("Placement exclusions applied: %d campaign negative placement criteria", len(placementIDs)))
		res.Steps = steps
	}

	// The campaign-level user-list exclusions (#475), covering every ad group — one
	// atomic campaignCriteria:mutate of negative user-list criteria.
	if len(pf.displayTargeting.excludedUserLists) > 0 {
		exclIDs, exclErr := c.createCampaignAudienceExclusions(ctx, campaignResource, campaignID, pf.displayTargeting.excludedUserLists)
		res.ExcludedAudienceCriteriaIDs = exclIDs
		if exclErr != nil {
			return res, exclErr
		}
		steps = append(steps, fmt.Sprintf("Audience exclusions applied: %d campaign negative user list criteria", len(exclIDs)))
		res.Steps = steps
	}

	// Once per ad group: the image assets, then the ad that references
	// them — two mutates, because Google has no call that does both. The asset ids are
	// carried on the result even when the ad half failed, so an operator can find assets
	// created with nothing referencing them.
	for k, g := range pf.displayGroups {
		if !g.creative.present {
			continue
		}
		if k > 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, fmt.Errorf("google-ads display creation aborted after the ads of %d of %d ad group(s) were created (campaign %s; context done before the ad of ad group %q): %w", k, len(pf.displayGroups), campaignID, g.name, ctxErr)
			}
		}
		assetIDs, adID, adErr := c.createDisplayAd(ctx, adGroupResources[k], adGroupIDs[k], pf.finalURL, g.creative, groupImages[k])
		if k == 0 {
			res.CreativeAssetIDs = assetIDs
		}
		if multi {
			res.AdGroups[k].CreativeAssetIDs = assetIDs
		}
		if adErr != nil {
			// Every ad group exists before this pass starts, so a stop on group k leaves
			// groups k+1..n created with NO ad. adGroups lists them with their ids and no
			// adIds; the error names them too, so the caller need not infer it.
			if later := pf.displayGroups[k+1:]; multi && len(later) > 0 {
				return res, fmt.Errorf("google-ads display ad pass stopped at ad group %q; "+
					"the later ad group(s) %s were created and have NO ad: %w",
					g.name, displayGroupNames(later), adErr)
			}
			return res, adErr
		}
		if k == 0 {
			res.AdID = adID
		}
		if multi {
			res.AdGroups[k].AdIDs = []string{adID}
		}
		steps = append(steps, fmt.Sprintf("Display ad created: %s (PAUSED, %s)", adID, displayCreativeStep(g.creative))+displayGroupStep(multi, g.name))
		res.Steps = steps
	}

	// The closing step reads the FIRST group's fields, and that is true for every group
	// (#475): geo is campaign-level, every named group must carry a creative, and the pass
	// above returns on the first failure — so reaching here with an ad means every group
	// got its ad.
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
		return "Display campaign created (no geo targeting set) — add geo targeting, then review the ad and launch in the Google Ads UI"
	case hasGeo:
		return "Display campaign created with NO AD — it cannot serve until one is added; add a responsive display ad in the Google Ads UI"
	default:
		return "Display campaign created with NO AD (no geo targeting set) — it cannot serve until one is added; add geo targeting and a responsive display ad in the Google Ads UI"
	}
}
