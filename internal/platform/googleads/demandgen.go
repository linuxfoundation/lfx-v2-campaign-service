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

// demandGenAdGroupName composes this channel's single ad group name.
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
// campaignCreate always sends `networkSettings` and `manualCpc`, and Demand Gen
// accepts neither (it bids with targetSpend and has no Search network to target).
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
	// strategies may be sent, and that lives in validateBiddingPlan — which refuses
	// everything except maximize clicks here, on recorded live-API evidence (see the
	// note at the campaign create below).
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
}

// demandGenAdGroupCreate omits the `type` field that the Search path sets to
// SEARCH_STANDARD. Demand Gen ad groups take the channel's own default; naming a
// Search ad-group type under a Demand Gen campaign is rejected.
type demandGenAdGroupCreate struct {
	Name     string `json:"name"`
	Campaign string `json:"campaign"`
	Status   string `json:"status"`
}

// CreateDemandGenCampaign creates a PAUSED Demand Gen campaign: budget → campaign →
// ad group → ad-group-level geo. It is a port of the legacy Express implementation
// (`lfx-self-serve` `campaign-proxy.service.ts`'s `createDemandGenCampaign`), which
// is what serves this channel today and is the behavioural reference.
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
// budget mutate, in fetchDemandGenImages. See demandgen_creative.go.
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
	images, err := c.fetchDemandGenImages(ctx, pf.creative)
	if err != nil {
		return nil, err // pre-create: nothing was sent
	}

	campaignName := pf.campaignName
	budgetName := pf.budgetName
	amountMicros := pf.amountMicros
	steps := []string{}

	// namePartial carries the names an operator needs to reconcile by, for a failure
	// that may have created something we never got an id for.
	namePartial := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "google-ads",
			AccountLabel: c.account.Label,
			// Stamped on every partial, not just the success result — a caller
			// reconciling a possibly-created campaign needs to know which account to
			// look in. Mirrors CreateCampaign's campaignNamePartial.
			CustomerID:         c.account.CustomerID,
			CampaignName:       campaignName,
			CampaignBudgetName: budgetName,
			Steps:              steps,
		}
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
	if err != nil {
		switch {
		case isDuplicateBudgetNameErr(err):
			return namePartial(), fmt.Errorf("google-ads demand gen budget %q already exists (DUPLICATE_NAME) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, err)
		case createOutcomeAmbiguous(err):
			return namePartial(), fmt.Errorf("google-ads demand gen budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		default:
			return nil, fmt.Errorf("google-ads demand gen budget creation failed: %w", err)
		}
	}
	budgetResource, budgetID, err := firstResourceName(budgetResp)
	if err != nil {
		return namePartial(), fmt.Errorf("google-ads demand gen budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))

	budgetPartial := func() *CampaignResult {
		r := namePartial()
		r.CampaignBudgetID = budgetID
		return r
	}

	// The budget is committed. A dead context here must surface it as reconcilable
	// rather than firing the campaign mutate blind.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return budgetPartial(), fmt.Errorf("google-ads demand gen creation aborted after budget %s created (context done before campaign create; the budget may need reconciling): %w", budgetID, ctxErr)
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
		StartDateTime: pf.startDateTime,
		EndDateTime:   pf.endDateTime,
		// Resolved by the SHARED preflight. On this channel the plan can only ever be
		// maximize clicks, so this renders `targetSpend: {}` — matching the legacy
		// Express implementation's `target_spend: {}`, which is what serves this channel
		// on app.lfx.dev today.
		//
		// VERIFIED against the live API (2026-08-14): a validateOnly campaigns:mutate at
		// v23 on a real account returned HTTP 200 for DEMAND_GEN + targetSpend. A review
		// had flagged it as unsupported on this channel and proposed maximizeConversions
		// instead; that payload returned HTTP 400
		// BIDDING_STRATEGY_TYPE_INCOMPATIBLE_WITH_SHARED_BUDGET against the same budget.
		// That recorded rejection is why demandGenBiddingStrategies holds exactly one
		// entry: the rejection lands AFTER the budget is created, which orphans it, so
		// widening the set is a live-API question and not a code-reading one. Re-run that
		// validateOnly check before adding a strategy there.
		biddingFields: pf.bidding.fields(),
	}}}}
	campaignResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			return budgetPartial(), fmt.Errorf("google-ads demand gen campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if err := c.validateCampaignResource(campaignResource); err != nil {
		return budgetPartial(), fmt.Errorf("google-ads demand gen campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, err)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, DEMAND_GEN, target spend, %s)", campaignID, flightWindowStep(pf.startDateTime, pf.endDateTime)))

	campaignPartial := func() *CampaignResult {
		r := budgetPartial()
		r.CampaignID = campaignID
		return r
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return campaignPartial(), fmt.Errorf("google-ads demand gen creation aborted after campaign %s created (context done before ad group create): %w", campaignID, ctxErr)
	}

	// Step 3: the ad group. Demand Gen ad groups take no explicit type.
	adGroupName := demandGenAdGroupName(in)
	adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: demandGenAdGroupCreate{
		Name:     adGroupName,
		Campaign: campaignResource,
		Status:   "ENABLED",
	}}}}
	// namePartial carries the deterministic ad-group NAME on every failure arm below. A
	// partial without it tells a reconciler an ad group may exist but not what to look
	// for, which is the difference between "verify this name in Google Ads" and a manual
	// hunt. Mirrors the Search path (adgroup_ad.go), whose messages all name the ad group.
	adGroupPartial := func() *CampaignResult {
		r := campaignPartial()
		r.AdGroupName = adGroupName
		return r
	}
	adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
	if err != nil {
		// Classify exactly as the Search path and the budget/campaign steps above do: a
		// duplicate name and an ambiguous transport failure both mean the ad group MAY
		// exist, so neither may be reported as a clean failure — the caller decides
		// whether to retry, and a flat "failed" invites a duplicate.
		switch {
		case isDuplicateAdGroupNameErr(err):
			return adGroupPartial(), fmt.Errorf("google-ads demand gen ad group %q already exists (DUPLICATE_ADGROUP_NAME) — a prior attempt likely created it; verify in Google Ads before retrying (campaign %s created): %w", adGroupName, campaignID, err)
		case createOutcomeAmbiguous(err):
			return adGroupPartial(), fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err)
		default:
			return adGroupPartial(), fmt.Errorf("google-ads demand gen ad group creation failed (campaign %s created): %w", campaignID, err)
		}
	}
	adGroupResource, adGroupID, err := firstResourceName(adGroupResp)
	if err != nil {
		return adGroupPartial(), fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, err)
	}
	// firstResourceName only extracts a trailing id; it does not check resource kind or
	// account. Without this, a malformed or wrong-account 2xx (another customer's adGroups
	// resource) would be accepted as confirmed and its id persisted as AdGroupID.
	if verr := c.validateResourceKind("adGroups", adGroupResource, true); verr != nil {
		return adGroupPartial(), fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying; campaign %s created): %w", adGroupName, campaignID, verr)
	}
	res := adGroupPartial()
	res.AdGroupID = adGroupID
	steps = append(steps, fmt.Sprintf("Ad group created: %s", adGroupID))
	res.Steps = steps

	// Step 4: geo. Location criteria go on the AD GROUP here, NOT the campaign —
	// Demand Gen rejects campaign-level location criteria, which is why this is a
	// different call from the Search path's (see geo.go). Both channels now resolve
	// the same caller-supplied country codes through the same map, so neither has
	// targeting the other lacks (LFXV2-3283).
	//
	// A failure is returned ALONGSIDE the non-nil res, like every step past the
	// campaign create: the campaign and ad group exist and are reconcilable either way.
	if !pf.geo.empty() {
		geoIDs, geoErr := c.createAdGroupGeoTargeting(ctx, adGroupResource, adGroupID, pf.geo)
		if geoErr != nil {
			return res, geoErr
		}
		res.GeoCriterionIDs = geoIDs
		steps = append(steps, fmt.Sprintf("Geo targeting applied: %d ad-group location criteria (%s)", len(geoIDs), geoStep(in, pf.geo)))
		res.Steps = steps
	}

	// Step 5: the creative. Images first as account-level assets, then the ad that
	// references them — two mutates, because Google has no call that does both.
	//
	// Like the geo step, a failure is returned ALONGSIDE the non-nil res: the campaign
	// and ad group exist either way, and the asset ids are carried even when the ad
	// itself failed so the operator can find images created with nothing referencing
	// them.
	if pf.creative.present {
		assetIDs, adID, adErr := c.createDemandGenAd(ctx, adGroupResource, adGroupID, pf.finalURL, pf.creative, images)
		res.CreativeAssetIDs = assetIDs
		if adErr != nil {
			return res, adErr
		}
		res.AdID = adID
		steps = append(steps, fmt.Sprintf("Demand Gen ad created: %s (PAUSED, %s)", adID, demandGenCreativeStep(pf.creative)))
		res.Steps = steps
	}

	// The closing step reports what is actually true of this campaign rather than a
	// fixed string. It used to say "no geo targeting set" unconditionally, which
	// became a lie on every targeted create; the same correction now applies to the
	// ad, which is no longer always absent.
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
		return "Demand Gen campaign created (no geo targeting set) — add targeting, then review the creative and launch in the Google Ads UI"
	case hasGeo:
		return "Demand Gen campaign created with NO AD — it cannot serve until an ad is added; upload images and publish in the Google Ads UI"
	default:
		return "Demand Gen campaign created with NO AD (no geo targeting set) — it cannot serve until an ad is added; add targeting, upload images and publish in the Google Ads UI"
	}
}
