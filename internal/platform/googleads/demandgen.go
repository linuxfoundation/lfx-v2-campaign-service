// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const (
	// advertisingChannelDemandGen is the channel type for Demand Gen campaigns —
	// YouTube, Discover, Gmail and Display inventory, as opposed to SEARCH.
	advertisingChannelDemandGen = "DEMAND_GEN"
)

// demandGenAdGroupName composes this channel's ad group name: the single group's, and the
// prefix every DemandGenAdGroups entry's label is appended to (#460).
//
// It exists as a function rather than an inline expression so the Display channel's test
// can assert the two names DIFFER by calling both compositions, instead of re-deriving
// this one as a literal that no later edit here would invalidate. Changing the suffix
// below must fail displayAdGroupName's collision test, not production.
//
// Deliberately still strings.TrimSpace where the three newer creative paths use
// sanitizeNamePart: Demand Gen is the one shipped channel, and sanitizeNamePart's
// whitespace-run collapse would rename ad groups already attached to live campaigns,
// defeating the name-based reconciliation this name exists for. The cost of that choice
// is that the control-character stranding at `adGroups:mutate` which sanitizeNamePart
// closes for Display, Video and PMax stays OPEN here — a known, accepted gap, not an
// oversight. Closing it needs a migration of the live names, not an edit to this line.
func demandGenAdGroupName(in CampaignInput) string {
	return strings.TrimSpace(in.EventName) + " - Display"
}

// demandGenCampaignCreate is a SEPARATE payload from campaignCreate rather than a
// widened version of it, because the two channels disagree on required fields:
// campaignCreate always sends `networkSettings`, and Demand Gen has no Search network
// to target; nor does it take `manualCpc`, which validateBiddingPlan never admits here.
// Sending the Search shape here is rejected by the API, and adding pointers to
// campaignCreate would make every Search create carry fields it must never omit.
type demandGenCampaignCreate struct {
	Name                           string `json:"name"`
	Status                         string `json:"status"`
	AdvertisingChannelType         string `json:"advertisingChannelType"`
	CampaignBudget                 string `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string `json:"containsEuPoliticalAdvertising"`
	// biddingFields is embedded ANONYMOUSLY, exactly as on campaignCreate, so the
	// oneof invariant has ONE definition rather than one per channel. It replaced a
	// fixed `targetSpend` field. What is channel-specific is not the shape but which
	// strategies may be sent, and that lives in validateBiddingPlan — see
	// demandGenBiddingStrategies and the note at the campaign create below.
	biddingFields
	// Carried even though this channel attaches its geo criteria at the AD GROUP level:
	// geoTargetTypeSetting is a CAMPAIGN-level field in the Google Ads API and governs how
	// location criteria are interpreted for the whole campaign, ad-group criteria included.
	// Without it Google defaults to PRESENCE_OR_INTEREST and a "US" ad group still serves to
	// anyone worldwide showing interest in the US — see the type's doc in campaign.go.
	//
	// This is one of the few fields the two channel payloads DO share; they are otherwise
	// separate because Demand Gen rejects networkSettings and manualCpc.
	GeoTargetTypeSetting geoTargetTypeSetting `json:"geoTargetTypeSetting"`
	// StartDateTime/EndDateTime are the same v23 campaign.start_date_time /
	// campaign.end_date_time fields campaignCreate carries, with the same format,
	// account-timezone interpretation and omitempty semantics — see that type for the
	// v23 rename trap. A flight window is a property of the campaign, not of the
	// channel, so both payloads take it from the one shared preflight: these are
	// CAMPAIGN-level fields in the Google Ads API, not Search-specific ones, and
	// Google's own Demand Gen create guide lists both as optional on this channel.
	//
	// Carrying them is not optional for correctness. A Demand Gen campaign that
	// validated a window and then did not send it would be worse than one that never
	// had dates: applyCampaignConfig records the window on the row either way, so the
	// campaigns table would claim an end date the campaign does not have, and the
	// settings readback reports `unknown` rather than `diverged` when one side is
	// absent — blinding the drift detector precisely where it is needed.
	StartDateTime string `json:"startDateTime,omitempty"`
	EndDateTime   string `json:"endDateTime,omitempty"`
	// URLCustomParameters is the utmcampaign custom parameter (#476), from the same
	// shared preflight as on every sibling payload; see campaignCreate.URLCustomParameters.
	URLCustomParameters []urlCustomParameter `json:"urlCustomParameters,omitempty"`
}

// demandGenAdGroupCreate omits the `type` field that the Search path sets to
// SEARCH_STANDARD. Demand Gen ad groups take the channel's own default; naming a
// Search ad-group type under a Demand Gen campaign is rejected.
type demandGenAdGroupCreate struct {
	Name     string `json:"name"`
	Campaign string `json:"campaign"`
	Status   string `json:"status"`
}

// CreateDemandGenCampaign creates a PAUSED Demand Gen campaign: the demographic Audience
// when DemandGenDemographics is set (#498), and one for each DemandGenAdGroups entry that sets
// Demographics (#518) → budget → campaign →
// conversion goal → campaign-level ad schedule and device criteria → sitelinks (#474) →
// ad group → ad-group-level geo → ad-group-level audience and languages (#459) → ad, the
// last four repeated per ad group when DemandGenAdGroups names several (#460). It is a port of the
// legacy Express implementation (`lfx-self-serve` `campaign-proxy.service.ts`'s
// `createDemandGenCampaign`), which is what serves this channel today and is the
// behavioural reference.
//
// It creates NO KEYWORDS: Demand Gen has no keyword criteria, and the preflight
// refuses them rather than dropping them.
//
// It DOES now create an ad, when the caller supplies CampaignInput.DemandGenCreative.
// This reverses a rationale this file used to state — that creating no ad was
// deliberate because "Demand Gen ads are image/video asset based, the assets are
// uploaded by a human in the Google Ads UI". Half of that was right and half was a
// non sequitur. The right half: a text ad would indeed be unservable here, so the
// Search path's responsive search ad could not simply be reused. The non sequitur:
// needing image assets is not a reason a human must supply them interactively. The
// real obstacle was that ImageAsset.data is bytes and Google will not fetch a URL,
// so the service has to download the images itself — which it now does, before the
// budget mutate, in fetchDemandGenGroupImages. See demandgen_creative.go. It can also
// create a VIDEO ad instead, from CampaignInput.DemandGenVideoCreative (#460); see
// demandgen_video_creative.go.
//
// With no creative supplied the old behaviour stands unchanged: campaign, ad group
// and geo, no ad, and a closing step that says the campaign cannot serve until an
// ad is added. That is kept because campaigns created before the creative existed
// are that shape, and ValidateCampaignInputKind must keep accepting them.
//
// The partial-result contract matches CreateCampaign exactly, and that is the part
// the legacy TS does NOT have: every step that may have committed upstream returns a
// NON-NIL result carrying what is known so far, so the orchestrator can distinguish
// "nothing was created" from "something may exist and needs reconciling". Returning
// (nil, err) here would release the claim on a campaign that exists and spends.
func (c *Client) CreateDemandGenCampaign(ctx context.Context, in CampaignInput) (*CampaignResult, error) {
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err != nil {
		return nil, err // pre-create: nothing was sent
	}

	// The images are downloaded BEFORE the first mutate, with everything else that
	// can fail for free. A 404, an over-sized file or an image that misses Google's
	// aspect ratio therefore costs nothing — the same orphan-avoidance guarantee the
	// pure validators in preflightCampaignKind give, extended over the one check that
	// genuinely needs the network. Nothing has been created yet, so this returns
	// (nil, err) like the preflight above and unlike every step after it.
	// Every ad group's images, and a video ad's logo, in one walk (#460); see
	// fetchDemandGenGroupImages.
	groupImages, err := c.fetchDemandGenGroupImages(ctx, pf.demandGenGroups)
	if err != nil {
		return nil, imageFetchRefusal(ctx, err) // pre-create: nothing was sent
	}

	campaignName := pf.campaignName
	budgetName := pf.budgetName
	amountMicros := pf.amountMicros
	steps := []string{}

	// The demographic Audiences (#498, #518), the last step before the budget, after the image
	// fetch so a bad image costs no Audience either: the campaign-level one first, then each ad
	// group's own, in entry order. No budget or campaign exists yet. A definite failure of the
	// first returns (nil, err): nothing was created. A create that may have landed (5xx,
	// timeout, an unusable 2xx), or any failure once an earlier Audience is in place, returns a
	// partial naming every Audience so far and the planned name of the failed one when it may
	// exist, so the claim is kept and a retry reuses each by name
	// (ensureDemandGenDemographicAudience).
	var demographicAudience, demographicAudienceName string
	var groupAudiences []DemandGenAdGroupAudience
	// groupAudience is each group's own Audience resource, by group index, "" for none.
	groupAudience := make([]string, len(pf.demandGenGroups))

	// namePartial carries the names an operator needs to reconcile by, for a failure
	// that may have created something we never got an id for.
	namePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:     platformGoogleAds,
			AccountLabel: c.account.Label,
			// Stamped on every partial, not just the success result — a caller
			// reconciling a possibly-created campaign needs to know which account to
			// look in. Mirrors CreateCampaign's campaignNamePartial.
			CustomerID:         c.account.CustomerID,
			CampaignName:       campaignName,
			CampaignBudgetName: budgetName,
			Steps:              steps,
			// The Audiences exist before the budget, so every partial names them.
			DemandGenDemographicAudience:     demographicAudience,
			DemandGenDemographicAudienceName: demographicAudienceName,
			DemandGenAdGroupAudiences:        groupAudiences,
		}
	}
	// audienceFailed is the result of a failed Audience create: (nil, err) when nothing at all
	// may exist, otherwise a partial naming what does.
	audienceFailed := func(unconfirmed bool, name, tag string, err error) (*CampaignResult, error) {
		if !unconfirmed && !demographicAudiencesInPlace(namePartial()) {
			return nil, err // definitely nothing new was created
		}
		if unconfirmed {
			steps = append(steps, fmt.Sprintf("Demographic Audience UNCONFIRMED: %q may exist; no budget or campaign was created", name)+tag)
		}
		return namePartial(), err
	}
	if pf.demandGenDemographics != nil {
		demographicAudienceName = pf.demandGenDemographics.create.Name
		resource, how, unconfirmed, audErr := c.ensureDemandGenDemographicAudience(ctx, pf.demandGenDemographics)
		if audErr != nil {
			return audienceFailed(unconfirmed, demographicAudienceName, "", audErr)
		}
		demographicAudience = resource
		steps = append(steps, demographicAudienceStep(how, resource, pf.demandGenDemographics))
	}
	for k, g := range pf.demandGenGroups {
		if g.demographics == nil {
			continue
		}
		resource, how, unconfirmed, audErr := c.ensureDemandGenDemographicAudience(ctx, g.demographics)
		if audErr != nil {
			if unconfirmed {
				groupAudiences = append(groupAudiences, DemandGenAdGroupAudience{AdGroupName: g.name, DemographicAudienceName: g.demographics.create.Name})
			}
			return audienceFailed(unconfirmed, g.demographics.create.Name, demandGenGroupStep(true, g.name), audErr)
		}
		groupAudience[k] = resource
		groupAudiences = append(groupAudiences, DemandGenAdGroupAudience{AdGroupName: g.name, DemographicAudience: resource, DemographicAudienceName: g.demographics.create.Name})
		steps = append(steps, demographicAudienceStep(how, resource, g.demographics))
	}

	// Step 1: budget. Same shape as the Search path — budgets are channel-agnostic.
	shared := false
	budgetReq := mutateRequest{Operations: []mutateOperation{{Create: campaignBudgetCreate{
		Name:             budgetName,
		AmountMicros:     amountMicros,
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
		budgetResource, budgetID, recovered, err = c.budgetFromCreateResponse(ctx, "demand gen", budgetName, amountMicros, budgetResp)
		if err != nil {
			return namePartial(), err
		}
		steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))
		if recovered != "" {
			steps = append(steps, recovered)
		}
	case isDuplicateBudgetNameErr(err):
		resource, id, reuseSteps, reuseErr := c.reuseBudgetAfterDuplicateName(ctx, budgetName, amountMicros, in.Budget)
		if reuseErr != nil {
			// BOTH errors are wrapped, and the DUPLICATE_NAME refusal goes FIRST
			// because that order is load-bearing: isDuplicateBudgetNameErr (and every
			// other errors.As predicate) stops at the FIRST *apiError in the chain, so
			// wrapping the reuse failure ahead of it would let an apiError from the
			// lookup READ shadow the duplicate code and cost this failure its precise
			// create-outcome token. carriesUnconfirmedWrite searches the whole chain,
			// so the reuse side is still read for a write that may have landed.
			return namePartial(), fmt.Errorf("google-ads demand gen budget %q already exists (DUPLICATE_NAME): %w — and could not be safely reused: %w — a prior attempt likely created it; verify in Google Ads before retrying", budgetName, err, reuseErr)
		}
		budgetResource, budgetID = resource, id
		steps = append(steps, reuseSteps...)
	case createOutcomeAmbiguous(err):
		// Resolved against the account rather than reported as unknown — see
		// ambiguousBudgetCreateOutcome; every channel takes this arm identically.
		return c.ambiguousBudgetCreateOutcome(ctx, "demand gen", budgetName, namePartial, err)
	default:
		if r := namePartial(); demographicAudiencesInPlace(r) {
			// The Audiences were created or reused before the budget, so the result must name them.
			return r, fmt.Errorf("google-ads demand gen budget creation failed (demographic Audience %s in place): %w", strings.Join(demographicAudienceResources(r), ", "), err)
		}
		return nil, fmt.Errorf("google-ads demand gen budget creation failed: %w", err)
	}

	budgetPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	// The budget is committed. A dead context here must surface it as reconcilable
	// rather than firing the campaign mutate blind.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads demand gen creation aborted after budget %s was created or reused (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
	}

	// Step 2: the campaign. PAUSED on create, like the Search path and the legacy
	// implementation — a created campaign never serves until a human enables it.
	campaignReq := mutateRequest{Operations: []mutateOperation{{Create: demandGenCampaignCreate{
		Name:                           campaignName,
		Status:                         "PAUSED",
		AdvertisingChannelType:         advertisingChannelDemandGen,
		CampaignBudget:                 budgetResource,
		ContainsEuPoliticalAdvertising: euPoliticalAdvertisingNo,
		GeoTargetTypeSetting:           geoTargetTypeSetting{PositiveGeoTargetType: geoTargetPresence},
		// Resolved by the SHARED preflight, so an invalid window is refused before the
		// budget mutate on this channel exactly as it is on Search.
		StartDateTime:       pf.startDateTime,
		EndDateTime:         pf.endDateTime,
		URLCustomParameters: pf.urlCustomParameters,
		// Resolved by the SHARED preflight. The default is maximize clicks, rendering
		// `targetSpend: {}` — matching the legacy Express implementation's
		// `target_spend: {}`, which is what serves this channel on app.lfx.dev today.
		//
		// HISTORY (2026-08-14): a validateOnly campaigns:mutate at v23 on a real account
		// returned HTTP 200 for DEMAND_GEN + targetSpend, and HTTP 400
		// BIDDING_STRATEGY_TYPE_INCOMPATIBLE_WITH_SHARED_BUDGET for maximizeConversions
		// against the same budget. That rejection kept demandGenBiddingStrategies to
		// maximize clicks alone until #474.
		//
		// NEW PROBE (2026-11-03, v23, #474): with a NON-shared budget — what every LFX
		// create makes, explicitlyShared false above — validateOnly accepted
		// `maximizeConversions: {}`, `targetCpa: {targetCpaMicros}` and
		// `maximizeConversionValue: {}`, and refused `maximizeConversions:
		// {targetCpaMicros}` (OPERATION_NOT_PERMITTED_FOR_CONTEXT). The 2026-08-14 refusal
		// was about a shared budget, not the channel. So the set now holds those four
		// strategies, and a target CPA renders as the standalone `targetCpa`
		// (biddingPlan.standaloneTargetCPA). Widening it further is still a live-API
		// question, not a code-reading one.
		biddingFields: pf.bidding.fields(),
	}}}}
	campaignResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads demand gen campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s in place) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation UNCONFIRMED (budget %s in place; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation failed (budget %s in place): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation UNCONFIRMED (budget %s in place; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if err := c.validateCampaignResource(campaignResource); err != nil {
		return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation UNCONFIRMED (budget %s in place; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, DEMAND_GEN, %s, %s)", campaignID, pf.bidding.describe(), flightWindowStep(pf.startDateTime, pf.endDateTime)))
	if pf.utmCampaignStep != "" {
		steps = append(steps, pf.utmCampaignStep)
	}

	// The campaign-level criteria and sitelink ids (#474), set by the two steps after the
	// conversion goal and carried on every later partial.
	var campaignCriterionIDs, extensionAssetIDs, extensionLinkIDs []string
	campaignPartial := func() *CampaignResult {
		r := budgetPartial()
		r.CampaignID = campaignID
		r.CampaignTargetingCriterionIDs = campaignCriterionIDs
		r.ExtensionAssetIDs = extensionAssetIDs
		r.ExtensionLinkIDs = extensionLinkIDs
		return r
	}

	// The campaign-level conversion goal (#436), first of the post-create steps so a failure
	// leaves as little built on the campaign as possible. Under maximize clicks the goal sets
	// what the campaign counts as a conversion and the step line says so; under a
	// conversion strategy (#474) it is what the campaign bids toward. With no
	// ConversionActions a conversion strategy bids toward the ACCOUNT's goals, as on every
	// other channel, so it needs conversions recorded there to have anything to optimize.
	goalStep, goalErr := c.applyCampaignConversionGoal(ctx, campaignID, pf.bidding)
	if goalErr != nil {
		return campaignPartial(), goalErr
	}
	if goalStep != "" {
		steps = append(steps, goalStep)
	}

	// Ad schedules and device criteria are CAMPAIGN criteria on Demand Gen (#474), unlike
	// its geo, Audience and languages, which go on each ad group below. The languages are
	// left out of this plan for that reason: Google refuses a campaign-level language here.
	// After the goal and before the ad groups, as on Search, so a failure has as little
	// built on it as possible. Same partial-result contract: the campaign exists either way.
	campaignCriteria := pf.criteria
	campaignCriteria.languages = nil
	if !campaignCriteria.empty() {
		critIDs, critErr := c.createCampaignTargetingCriteria(ctx, campaignResource, campaignID, campaignCriteria)
		if critErr != nil {
			return campaignPartial(), critErr
		}
		campaignCriterionIDs = critIDs
		steps = append(steps, fmt.Sprintf("Campaign targeting applied: %d criteria (%s)", len(critIDs), criteriaStep(campaignCriteria)))
	}

	// Sitelinks (#474), the one extension type Demand Gen takes, created and linked to the
	// campaign by the Search path's createCampaignAssets. The asset ids are reported even
	// when the linking half fails, as on Search.
	if !pf.assets.empty() {
		assetIDs, linkIDs, assetErr := c.createCampaignAssets(ctx, campaignResource, campaignID, pf.assets)
		extensionAssetIDs = assetIDs
		if assetErr != nil {
			return campaignPartial(), assetErr
		}
		extensionLinkIDs = linkIDs
		steps = append(steps, fmt.Sprintf("Ad extensions applied: %d assets (%s)", len(linkIDs), assetStep(pf.assets)))
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return campaignPartial(), fmt.Errorf("google-ads demand gen creation aborted after campaign %s created (context done before ad group create): %w", campaignID, ctxErr)
	}

	// Steps 3-6, once per ad group (#460): the ad group, its geo, its Audience and
	// languages, and its ads. pf.demandGenGroups holds ONE group when the caller named none,
	// built from the campaign-level fields, and for that group with one ad every request,
	// step line and result field is what this cascade produced before the list existed.
	//
	// With a list, or a single group carrying several ads (#474), each group gets an
	// AdGroups entry appended BEFORE its mutate, as on the Search cascade, so a failure on
	// group k returns groups 1..k-1 complete and what exists of k, and every ad id has a
	// place for the launch toggle to find it; the scalar ad group fields stay the FIRST
	// group's. Every failure below is returned ALONGSIDE the non-nil res: the campaign
	// exists either way.
	multi := len(in.DemandGenAdGroups) > 0
	record := multi || len(pf.demandGenGroups[0].ads) > 1
	var res *CampaignResult
	fail := func(err error) (*CampaignResult, error) {
		res.Steps = steps
		return res, err
	}
	for k, g := range pf.demandGenGroups {
		if k > 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fail(fmt.Errorf("google-ads demand gen creation aborted after %d of %d ad group(s) were created (campaign %s; context done before ad group %q): %w", k, len(pf.demandGenGroups), campaignID, g.name, ctxErr))
			}
		}
		tag := demandGenGroupStep(multi, g.name)

		// Step 3: the ad group. Demand Gen ad groups take no explicit type.
		adGroupName := g.name
		adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: demandGenAdGroupCreate{
			Name:     adGroupName,
			Campaign: campaignResource,
			Status:   "ENABLED",
		}}}}
		// The result carries the deterministic ad-group NAME on every failure arm below. A
		// partial without it tells a reconciler an ad group may exist but not what to look
		// for, which is the difference between "verify this name in Google Ads" and a manual
		// hunt. Mirrors the Search path (adgroup_ad.go), whose messages all name the ad group.
		if res == nil {
			res = campaignPartial()
			res.AdGroupName = adGroupName
		}
		var entry *AdGroupResult
		if record {
			// PlannedAds (#474) is set now, before any ad exists, so a group whose ad j
			// fails reads as incomplete to the launch toggle rather than as a group of j-1.
			res.AdGroups = append(res.AdGroups, AdGroupResult{Name: adGroupName, PlannedAds: len(g.ads)})
			entry = &res.AdGroups[len(res.AdGroups)-1]
		}
		adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
		if err != nil {
			// Classify exactly as the Search path and the budget/campaign steps above do: a
			// duplicate name and an ambiguous transport failure both mean the ad group MAY
			// exist, so neither may be reported as a clean failure — the caller decides
			// whether to retry, and a flat "failed" invites a duplicate.
			switch {
			case isDuplicateAdGroupNameErr(err):
				return fail(fmt.Errorf("google-ads demand gen ad group %q already exists (DUPLICATE_ADGROUP_NAME) — a prior attempt likely created it; verify in Google Ads before retrying (campaign %s created): %w", adGroupName, campaignID, err))
			case createOutcomeAmbiguous(err):
				return fail(fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err))
			default:
				return fail(fmt.Errorf("google-ads demand gen ad group creation failed (campaign %s created): %w", campaignID, err))
			}
		}
		adGroupResource, adGroupID, err := firstResourceName(adGroupResp)
		if err != nil {
			return fail(fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err))
		}
		// firstResourceName only extracts a trailing id; it does not check resource kind or
		// account. Without this, a malformed or wrong-account 2xx (another customer's adGroups
		// resource) would be accepted as confirmed and its id persisted as AdGroupID.
		if verr := c.validateResourceKind("adGroups", adGroupResource, true); verr != nil {
			return fail(fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, verr))
		}
		if k == 0 {
			res.AdGroupID = adGroupID
		}
		if entry != nil {
			entry.ID = adGroupID
		}
		steps = append(steps, fmt.Sprintf("Ad group created: %s", adGroupID)+tag)
		res.Steps = steps

		// Step 4: geo. Location criteria go on the AD GROUP here, NOT the campaign —
		// Demand Gen rejects campaign-level location criteria, which is why this is a
		// different call from the Search path's (see geo.go). Both channels now resolve
		// the same caller-supplied country codes through the same map, so neither has
		// targeting the other lacks (LFXV2-3283). The same geo goes on EVERY group; see
		// DemandGenAdGroup for why.
		if !pf.geo.empty() {
			geoIDs, geoErr := c.createAdGroupGeoTargeting(ctx, adGroupResource, adGroupID, pf.geo)
			if geoErr != nil {
				return fail(geoErr)
			}
			if k == 0 {
				res.GeoCriterionIDs = geoIDs
			}
			if entry != nil {
				entry.GeoCriterionIDs = geoIDs
			}
			steps = append(steps, fmt.Sprintf("Geo targeting applied: %d ad-group location criteria (%s)", len(geoIDs), geoStep(in, pf.geo))+tag)
			res.Steps = steps
		}

		// Step 5: the group's Audience and the campaign's languages (#459), on the AD GROUP —
		// the only level Demand Gen takes either at (see demandgen_targeting.go for the probe
		// facts). Its own mutate after geo rather than inside geo's;
		// createDemandGenAdGroupTargeting says why.
		// The group's own demographic Audience (#518) first; otherwise the campaign-level one
		// (#498), which goes on every other group. The preflight refused either beside the
		// group's own Audience, so neither ever replaces one.
		audience := g.audience
		switch {
		case groupAudience[k] != "":
			audience = groupAudience[k]
		case demographicAudience != "":
			audience = demographicAudience
		}
		if audience != "" || len(pf.criteria.languages) > 0 {
			audienceIDs, languageIDs, targetErr := c.createDemandGenAdGroupTargeting(ctx, adGroupResource, adGroupID, audience, pf.criteria.languages)
			if targetErr != nil {
				return fail(targetErr)
			}
			if k == 0 {
				res.AudienceCriteriaIDs = audienceIDs
				res.TargetingCriterionIDs = languageIDs
			}
			if entry != nil {
				entry.AudienceCriteriaIDs = audienceIDs
				entry.TargetingCriterionIDs = languageIDs
			}
			steps = append(steps, fmt.Sprintf("Audience and language targeting applied: %d ad-group criteria (%s)", len(audienceIDs)+len(languageIDs), demandGenTargetingStep(audience, pf.criteria.languages))+tag)
			res.Steps = steps
		}

		// Step 6: the ads, in order (#474) — each an image ad (images first as account-level
		// assets, then the ad that references them) or a video ad (#460; logo first, then
		// the videos, the call to action and the ad in one atomic googleAds:mutate). The
		// preflight admits exactly one of the two per ad.
		//
		// The asset ids are carried even when the ad itself failed, so the operator can find
		// assets created with nothing referencing them; a failure on ad j returns ads
		// 1..j-1 complete. They are recorded PER AD (#474): each ad gets an entry.Ads entry,
		// appended before its requests, holding its own assets and, once created, its id. The
		// scalar CreativeAssetIDs and the entry's own are the first ad's alone, beside AdID,
		// so "assets with no ad id" keeps naming the ad they belong to.
		for j, ad := range g.ads {
			if j > 0 {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fail(fmt.Errorf("google-ads demand gen creation aborted after %d of %d ad(s) were created in ad group %s (campaign %s; context done before ad %q): %w", j, len(g.ads), adGroupID, campaignID, ad.label, ctxErr))
				}
			}
			var (
				assetIDs []string
				adID     string
				adErr    error
				adStep   string
				adEntry  *DemandGenAdResult
			)
			if entry != nil {
				entry.Ads = append(entry.Ads, DemandGenAdResult{Name: ad.label})
				adEntry = &entry.Ads[len(entry.Ads)-1]
			}
			if ad.creative.present {
				assetIDs, adID, adErr = c.createDemandGenAd(ctx, adGroupResource, adGroupID, pf.finalURL, ad.creative, groupImages[k][j].images)
				adStep = fmt.Sprintf("Demand Gen ad created: %s (PAUSED, %s)", adID, demandGenCreativeStep(ad.creative))
			} else {
				assetIDs, adID, adErr = c.createDemandGenVideoAd(ctx, adGroupResource, adGroupID, ad.adName, pf.finalURL, ad.video, groupImages[k][j].logos)
				adStep = fmt.Sprintf("Demand Gen video ad created: %s (PAUSED, %s)", adID, demandGenVideoCreativeStep(ad.video))
			}
			if k == 0 && j == 0 {
				res.CreativeAssetIDs = assetIDs
			}
			if entry != nil {
				if j == 0 {
					entry.CreativeAssetIDs = assetIDs
				}
				adEntry.CreativeAssetIDs = assetIDs
			}
			if adErr != nil {
				return fail(adErr)
			}
			if k == 0 && j == 0 {
				res.AdID = adID
			}
			if entry != nil {
				entry.AdIDs = append(entry.AdIDs, adID)
				adEntry.ID = adID
			}
			steps = append(steps, adStep+demandGenAdStep(ad.label)+tag)
			res.Steps = steps
		}
	}

	// The closing step reports what is actually true of this campaign rather than a
	// fixed string. It used to say "no geo targeting set" unconditionally, which
	// became a lie on every targeted create; the same correction now applies to the
	// ad, which is no longer always absent.
	//
	// It reads the FIRST group's fields, and that is true for every group (#460): the same
	// geo goes on each, every named group must carry a creative, and the loop above
	// returns on the first failure — so reaching here means every group got its ads.
	steps = append(steps, demandGenClosingStep(len(res.GeoCriterionIDs) > 0, res.AdID != ""))
	res.Steps = steps
	return res, nil
}

// demandGenClosingStep states what still has to happen by hand before the campaign
// can serve. Both halves are conditional because both can now go either way, and a
// closing step that overstates what was done is the specific failure this wording
// exists to avoid: an operator who reads "campaign created" and believes it is
// ready will un-pause a campaign that cannot serve.
func demandGenClosingStep(hasGeo, hasAd bool) string {
	switch {
	case hasGeo && hasAd:
		return "Demand Gen campaign created — review the creative and launch in the Google Ads UI"
	case hasAd:
		return "Demand Gen campaign created (no geo targeting set) — add geo targeting, then review the creative and launch in the Google Ads UI"
	case hasGeo:
		return "Demand Gen campaign created with NO AD — it cannot serve until an ad is added; upload images and publish in the Google Ads UI"
	default:
		return "Demand Gen campaign created with NO AD (no geo targeting set) — it cannot serve until an ad is added; add geo targeting, upload images and publish in the Google Ads UI"
	}
}
