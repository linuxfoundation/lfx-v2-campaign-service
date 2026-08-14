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

// demandGenCampaignCreate is a SEPARATE payload from campaignCreate rather than a
// widened version of it, because the two channels disagree on required fields:
// campaignCreate always sends `networkSettings` and `manualCpc`, and Demand Gen
// accepts neither (it bids with targetSpend and has no Search network to target).
// Sending the Search shape here is rejected by the API, and adding pointers to
// campaignCreate would make every Search create carry fields it must never omit.
type demandGenCampaignCreate struct {
	Name                           string         `json:"name"`
	Status                         string         `json:"status"`
	AdvertisingChannelType         string         `json:"advertisingChannelType"`
	CampaignBudget                 string         `json:"campaignBudget"`
	ContainsEuPoliticalAdvertising string         `json:"containsEuPoliticalAdvertising"`
	TargetSpend                    map[string]any `json:"targetSpend"`
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
// It deliberately creates NO AD and NO KEYWORDS. That is not an omission: Demand Gen
// ads are image/video asset based, the assets are uploaded by a human in the Google
// Ads UI, and the legacy path ends the same way with "upload images and publish in
// Google Ads UI". Generating a text ad here would produce an ad the channel cannot
// serve.
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
		// targetSpend, matching the legacy Express implementation's `target_spend: {}` —
		// which is what serves this channel on app.lfx.dev today, so it is evidence of what
		// the API accepts rather than a reading of the docs. A review flagged it as
		// unsupported for Demand Gen on v23; keeping the shape the working implementation
		// uses, and noting the disagreement here so a real create settles it. This is the
		// one field a live create would most usefully verify.
		TargetSpend: map[string]any{},
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
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, DEMAND_GEN, target spend)", campaignID))

	campaignPartial := func() *CampaignResult {
		r := budgetPartial()
		r.CampaignID = campaignID
		return r
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return campaignPartial(), fmt.Errorf("google-ads demand gen creation aborted after campaign %s created (context done before ad group create): %w", campaignID, ctxErr)
	}

	// Step 3: the ad group. Demand Gen ad groups take no explicit type.
	adGroupName := strings.TrimSpace(in.EventName) + " - Display"
	adGroupReq := mutateRequest{Operations: []mutateOperation{{Create: demandGenAdGroupCreate{
		Name:     adGroupName,
		Campaign: campaignResource,
		Status:   "ENABLED",
	}}}}
	adGroupResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), adGroupReq, false)
	if err != nil {
		return campaignPartial(), fmt.Errorf("google-ads demand gen ad group creation failed (campaign %s created): %w", campaignID, err)
	}
	_, adGroupID, err := firstResourceName(adGroupResp)
	if err != nil {
		return campaignPartial(), fmt.Errorf("google-ads demand gen ad group creation UNCONFIRMED (campaign %s created): %w", campaignID, err)
	}
	res := campaignPartial()
	res.AdGroupName = adGroupName
	res.AdGroupID = adGroupID
	steps = append(steps, fmt.Sprintf("Ad group created: %s", adGroupID))
	res.Steps = steps

	// GEO IS DELIBERATELY NOT SET HERE.
	//
	// The legacy implementation attaches location criteria at AD-GROUP level (Demand
	// Gen rejects campaign-level ones) using its own GEO_TARGET_MAP. This client has
	// no geo support at all — `CampaignInput` carries no GeoTargets field and the
	// Search path sets no location criteria either — so adding a Demand-Gen-only geo
	// path would mean inventing a geo-constant mapping that the rest of the client
	// does not have, and quietly giving one channel targeting the other lacks.
	//
	// Parity with the legacy path on geo is its own change, applying to BOTH channels.
	// Until then a created campaign is geo-untargeted and the closing step says so, so
	// nobody reads the absence as targeting that was applied.

	// No ad and no keywords, deliberately — see the doc comment.
	steps = append(steps, "Demand Gen campaign created (no geo targeting set) — add targeting, upload images and publish in the Google Ads UI")
	res.Steps = steps
	return res, nil
}
