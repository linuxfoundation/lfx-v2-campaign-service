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

	// assetAutomationFinalURLExpansion is the AssetAutomationType that carries final-URL
	// expansion at v23, and assetAutomationOptedOut the status that turns it off. v23
	// removed `url_expansion_opt_out` from the campaign and the asset group alike, but it
	// did NOT remove the control: it moved into `campaign.asset_automation_settings`,
	// which is settable at CREATE and supported on this channel.
	//
	// VERIFIED against the live API (2026-10-19), validateOnly so nothing was created:
	// `googleAdsFields:search` for `%asset_automation%` returns
	// `campaign.asset_automation_settings` (google.ads.googleads.v23.resources.
	// Campaign.AssetAutomationSetting), and a `campaigns:mutate` of the probe-D payload
	// plus the setting below validated 200. A deliberately bogus type on the same payload
	// is rejected naming `AssetAutomationTypeEnum.AssetAutomationType`, so the value below
	// is a real member of that enum and not a string Google ignored.
	assetAutomationFinalURLExpansion = "FINAL_URL_EXPANSION_TEXT_ASSET_AUTOMATION"
	assetAutomationOptedOut          = "OPTED_OUT"

	// expansionOptedOutStep records, for a campaign THIS request created, that the opt-out
	// went out with the create. It is a plain step and not an ACTION REQUIRED: the setting
	// is in the payload below, so there is nothing for an operator to do. It stays in
	// `Steps` because the operator reading them is often looking at a partial failure and
	// finishing the work by hand, and "expansion is already off" is the thing they would
	// otherwise go and check.
	expansionOptedOutStep = "Final-URL expansion: OPTED OUT at create (campaign.asset_automation_settings), so expanded URLs cannot strip the UTM attribution the data pipeline parses back out"

	// expansionActionRequiredIfExists is for the three arms that cannot confirm the
	// campaign: a duplicate BUDGET name (the budget name is this client's idempotency key,
	// so a re-dispatch fails there before campaigns:mutate is attempted, and the attempt
	// that created that budget may have got past Step 2), DUPLICATE_CAMPAIGN_NAME (a prior
	// attempt created it, and that attempt may well have died before printing this step)
	// and an ambiguous outcome.
	//
	// It survives the opt-out above, and the reason is the whole point of the hedge: the
	// campaign these arms are talking about was created by SOME EARLIER ATTEMPT, which may
	// predate this service ever sending the setting. All three already send the operator
	// to the Google Ads UI to check; if what is there was created without the opt-out,
	// expansion is on and nothing else will say so.
	//
	// The 2026-10-16 prod dispatches are NOT examples of that campaign, and this doc used
	// to cite them as if they were. Both died at `campaigns:mutate` with HTTP 400 — probe
	// A below is that same payload rejected outright as INVALID_ARGUMENT on the removed
	// `urlExpansionOptOut` field — so neither left a campaign for an operator to find.
	// What they left is an orphan BUDGET, which is the duplicate-budget arm's subject and
	// a different thing. The attempt this hedge is about is a hypothetical older one, not
	// a recorded one; keeping a real incident attached to it made the premise read as
	// established when it is precautionary.
	expansionActionRequiredIfExists = "ACTION REQUIRED: if that campaign exists, check its final-URL expansion in the Google Ads UI and turn it OFF if it is on — this service now opts out at create, but a campaign left by an earlier attempt may predate that, and expanded URLs carry none of the UTM attribution the data pipeline parses back out"
)

// assetAutomationSetting is one entry of `campaign.asset_automation_settings`. Both
// fields are sent unconditionally — no `omitempty` — for the reason
// BrandGuidelinesEnabled states: an omitted field leaves the outcome to Google's own
// default rather than to this service, and the default here is the behaviour the opt-out
// exists to prevent.
type assetAutomationSetting struct {
	AssetAutomationType   string `json:"assetAutomationType"`
	AssetAutomationStatus string `json:"assetAutomationStatus"`
}

// performanceMaxCampaignCreate is a THIRD payload shape rather than a widened
// campaignCreate, for the reason demandGenCampaignCreate states: the channels disagree
// on required fields. Performance Max rejects `networkSettings` (it has no network to
// choose — it serves on all of them by definition) and rejects `manualCpc`, and it is
// the only one of the five channel payloads that carries `brandGuidelinesEnabled`.
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
	// BrandGuidelinesEnabled is sent EXPLICITLY as false, and no `omitempty` guards it:
	// the whole point is that `false` reaches Google, and an omitted field would take the
	// account's own setting instead.
	//
	// VERIFIED against the live API (2026-10-16), the way demandgen.go records its own
	// validateOnly result. A prod dispatch on 2026-10-16 created the budget and then lost
	// the campaign to HTTP 400, twice; the probes below ran the same day, against the real
	// account, all of them validateOnly so nothing was created. Same payload each time bar
	// one field. A, B and D are the probe scripts' own labels, kept so this comment and
	// the full table in docs/knowledge/log/2026-10-19-LFXV2-2665-pmax-v23-create.md name
	// the same probes; these three are the ones the conclusion rests on, and the gap where
	// C would be is that label never having been used, not a probe omitted here.
	//
	//   A. the prod payload as sent          → 400 INVALID_ARGUMENT, Unknown name
	//                                          "urlExpansionOptOut" at 'operations[0].create'
	//   B. A minus urlExpansionOptOut        → 400 REQUIRED_BUSINESS_NAME_ASSET_NOT_LINKED
	//                                          + REQUIRED_LOGO_ASSET_NOT_LINKED
	//   D. B plus brandGuidelinesEnabled:false → VALIDATED OK
	//
	// So two blockers were stacked, and B is this field's. The account has Brand
	// Guidelines on; a Performance Max campaign created under it must link a
	// BUSINESS_NAME and a square LOGO as CAMPAIGN assets — Google's own error text says
	// "linked as a CampaignAsset".
	//
	// The gap is NOT that this service cannot build those two assets: pmax_creative.go
	// builds both, `assetFieldBusinessName` and `assetFieldLogo`, with the square logo's
	// geometry bound already enforced. The gap is WHERE and WHEN. They are linked as
	// ASSET-GROUP assets, through `assetGroupAssets:mutate`, and that happens at Step 5 —
	// AFTER the Step 2 campaign create this payload is the body of. So satisfying Brand
	// Guidelines needs a campaign-level `campaignAssets:mutate` link AND a reordering of
	// the cascade so the assets exist before the campaign is created; adding the link
	// step alone would still run too late. That is the work this field stands in for.
	//
	// Sending `false` creates the campaign without that enforcement. Do NOT read that as
	// "and it can be switched back on afterwards": Brand Guidelines is not an ordinary
	// mutable campaign field, nothing here has verified what re-enabling it on an existing
	// campaign costs, and this comment will outlive the workaround. Treat the setting as
	// decided at create time. Doing the reordering here is the better answer and is what
	// should replace this field.
	//
	BrandGuidelinesEnabled bool `json:"brandGuidelinesEnabled"`
	// AssetAutomationSettings carries the final-URL-expansion opt-out, which is what this
	// payload used to spell `urlExpansionOptOut: true` — stopping Performance Max from
	// crawling the site, generating its own landing pages, and spending the event's budget
	// on URLs carrying none of the UTM attribution the data pipeline parses back out.
	//
	// Probe A is that FIELD being gone at v23, removed outright on the campaign and the
	// asset group alike (`googleAdsFields:search` for `%url_expansion%` returns only the
	// reporting view `final_url_expansion_asset_view`). The CONTROL was not removed with
	// it — it moved to `campaign.asset_automation_settings`, verified live and settable at
	// create; see the consts at the top of this file for the two probes that establish it.
	//
	// So this is not a like-for-like rename and must not be read as one: the entry below
	// opts out of ONE asset-automation type, and Google may add others that this payload
	// then leaves at their default.
	//
	// Dropping `omitempty` here is for CONSISTENCY with the scalar above, not for the same
	// reason, and an earlier revision of this comment claimed otherwise. On
	// `BrandGuidelinesEnabled` the tag genuinely decides an outcome: `omitempty` would
	// elide `false` and hand the account's setting the decision. On a repeated field it
	// decides nothing — `encoding/json` writes a nil slice as `null` and an empty one as
	// `[]`, and proto3 JSON reads both exactly as an unset repeated field, which is also
	// what omitting the key does. All three spellings mean "no opt-out". The thing that
	// actually guarantees the opt-out ships is the one-element slice literal at the
	// `campaigns:mutate` call below, and it is pinned by
	// TestCreatePerformanceMaxCampaign_CampaignShape — so that test, not this tag, is what
	// a future edit has to get past.
	AssetAutomationSettings []assetAutomationSetting `json:"assetAutomationSettings"`
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
	//
	// Guarded on `present` the way Demand Gen and Display guard theirs: with no asset
	// group asked for there are no URLs, and the fetch would be a walk over an empty
	// slot list whose only effect is to spend the caller's deadline before the budget
	// mutate. The guard is also what keeps this call and the Step 5 `pf.pmax.present`
	// arm reading off the SAME condition — fetching unconditionally for a group that is
	// never created is the shape that invites someone to make the two disagree.
	var images []fetchedImage
	if pf.pmax.present {
		images, err = c.fetchSlotImages(ctx, performanceMaxImageSlots, pf.pmax.urls)
		if err != nil {
			return nil, err // pre-create: nothing was sent
		}
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
	// The arms are a single switch rather than `if err != nil { switch }` so the
	// DUPLICATE_NAME arm can resolve to a usable budget instead of only to an error; see
	// reuseBudgetAfterDuplicateName.
	var budgetResource, budgetID string
	switch {
	case err == nil:
		budgetResource, budgetID, err = firstResourceName(budgetResp)
		if err != nil {
			return namePartial(), fmt.Errorf("google-ads performance max budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
		}
		steps = append(steps, fmt.Sprintf("Campaign budget created: %s (%.2f/day in account currency)", budgetID, in.Budget))
	case isDuplicateBudgetNameErr(err):
		// FIRST, try to ADOPT the budget holding the name. The reasoning below says this
		// arm is reached only on an UNATTACHED budget; reuseBudgetAfterDuplicateName
		// proves it instead of assuming it, and on proof the dispatch continues onto that
		// budget rather than dead-ending. It matters most on exactly this channel: both
		// 2026-10-16 prod dispatches and the 2026-10-20 retry died at campaigns:mutate
		// here, and each one orphaned a budget whose name then blocked the next attempt.
		resource, id, reuseSteps, reuseErr := c.reuseBudgetAfterDuplicateName(ctx, budgetName, pf.amountMicros, in.Budget)
		if reuseErr == nil {
			budgetResource, budgetID = resource, id
			steps = append(steps, reuseSteps...)
			// No expansion hedge on this path. The hedge below exists because an
			// unprovable campaign MIGHT be sitting under this name; a successful reuse has
			// just proved no campaign references this budget, so there is nothing to warn
			// about.
			break
		}
		// Hedged, one mutate earlier than the campaign arms below, and hedged on a
		// NARROW premise — narrow enough to be worth stating, because the obvious
		// reading of it is wrong twice over.
		//
		// The budget name is this client's idempotency key, so a re-dispatch of the
		// same brief on this channel fails HERE, before campaigns:mutate is attempted.
		// The tempting conclusion — "so the attempt that created this budget may have
		// got past Step 2, leaving a campaign" — does not follow: this budget is
		// created non-shared (`shared := false` above), and a non-shared budget's name
		// SYNCHRONIZES to the campaign name once a campaign attaches to it
		// (campaign.go's `campaignNamePartial` contract, which `internal/dispatch/
		// googleads.go` reconciles by id for exactly this reason). An attempt that got
		// past Step 2 therefore FREED `budgetName`, and this arm would not have been
		// reached at all. Reaching it means an UNATTACHED budget — the orphan shape,
		// which is what the two 2026-10-16 prod dispatches actually left.
		//
		// Nor would the campaign be "a campaign with expansion on": this file now opts
		// out at create (the `AssetAutomationSettings` literal in Step 2), so anything
		// the CURRENT code leaves behind has expansion OFF.
		//
		// What survives both corrections, and is the only reason the step is still
		// appended: the sync is Google's behaviour as this package documents it, not
		// something this service verified, and nothing here has established what
		// happens to a non-shared budget's name if its campaign is later removed. A
		// campaign predating this file's opt-out could also still be sitting under a
		// re-created budget of the original name. Both are narrow, neither is disproved,
		// and the cost of the hedge is one UI check the operator is already being sent
		// to make. The two arms below stay silent because there the budget itself is
		// the open question and the campaign create provably was never reached.
		steps = append(steps, expansionActionRequiredIfExists)
		return namePartial(), fmt.Errorf("google-ads performance max budget %q already exists (DUPLICATE_NAME) and could not be safely reused (%v) — a prior attempt likely created it; verify in Google Ads before retrying: %w", budgetName, reuseErr, err)
	case createOutcomeAmbiguous(err):
		return namePartial(), fmt.Errorf("google-ads performance max budget creation UNCONFIRMED (%q may exist — verify in Google Ads before retrying): %w", budgetName, err)
	default:
		return nil, fmt.Errorf("google-ads performance max budget creation failed: %w", err)
	}

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
		BrandGuidelinesEnabled:         false,
		AssetAutomationSettings: []assetAutomationSetting{{
			AssetAutomationType:   assetAutomationFinalURLExpansion,
			AssetAutomationStatus: assetAutomationOptedOut,
		}},
		StartDateTime: pf.startDateTime,
		EndDateTime:   pf.endDateTime,
		// Resolved by the SHARED preflight, which on this channel admits only the four
		// conversion-based strategies — Performance Max has no manual bidding at all.
		biddingFields: pf.bidding.fields(),
	}}}}
	campaignResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaigns:mutate"), campaignReq, false)
	if err != nil {
		switch {
		case isDuplicateCampaignNameErr(err):
			// Hedged, not omitted: the name is taken, so a campaign by this name exists —
			// created by an attempt that may itself have died before reaching the step below.
			steps = append(steps, expansionActionRequiredIfExists)
			return budgetPartial(), fmt.Errorf("google-ads performance max campaign %q already exists (DUPLICATE_CAMPAIGN_NAME; budget %s created) — a prior attempt likely created it; verify in Google Ads before retrying: %w", campaignName, budgetID, err)
		case createOutcomeAmbiguous(err):
			steps = append(steps, expansionActionRequiredIfExists)
			return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation UNCONFIRMED (budget %s created; campaign %q may exist — verify in Google Ads before retrying): %w", budgetID, campaignName, err)
		default:
			// The one arm that stays silent: a definite non-ambiguous error means no campaign
			// was created, so there is nothing whose expansion setting could be wrong.
			return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation failed (budget %s created): %w", budgetID, err)
		}
	}
	// PAST THIS LINE THE CAMPAIGN EXISTS. `campaigns:mutate` answered 2xx, so the two
	// failures below are a resource name this client cannot use — not a campaign that was
	// never created. They hand back no CampaignID, which makes these Steps the only thing
	// that tells the operator a billable campaign is out there at all.
	campaignResource, campaignID, err := firstResourceName(campaignResp)
	if err != nil {
		steps = append(steps, expansionOptedOutStep)
		return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation UNCONFIRMED (budget %s created; 2xx with no/malformed resource name — verify in Google Ads before retrying): %w", budgetID, err)
	}
	if verr := c.validateCampaignResource(campaignResource); verr != nil {
		steps = append(steps, expansionOptedOutStep)
		return budgetPartial(), fmt.Errorf("google-ads performance max campaign creation UNCONFIRMED (budget %s created; malformed campaign resource name %q — verify in Google Ads before retrying): %w", budgetID, campaignResource, verr)
	}
	steps = append(steps, fmt.Sprintf("Campaign created: %s (PAUSED, PERFORMANCE_MAX, %s, %s)", campaignID, pf.bidding.describe(), flightWindowStep(pf.startDateTime, pf.endDateTime)))
	// HERE, immediately after the campaign is confirmed, and not down beside the closing
	// step. Every return below this line hands back a campaign that is already billable, and
	// internal/dispatch/googleads.go keeps and persists the claim on a non-nil result — so a
	// partial failure puts an operator in front of exactly these Steps, finishing the geo or
	// asset-group work by hand in the Google Ads UI and launching from there. Appended at the
	// end, this step would reach only the operator least likely to need it. The returns ABOVE
	// this line carry it too, for the same reason: a 2xx with an unusable resource name is
	// still a campaign, and it is still one this request opted out at create.
	steps = append(steps, expansionOptedOutStep)

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

	// The gate Video and Display both carry, and the one this cascade was missing. The
	// campaign exists and is billable from here on, so a context that has already been
	// cancelled must be reported ALONGSIDE the result rather than discovered halfway
	// through the asset-group mutates — where it would strand a group with some of its
	// assets linked and a count the activation gate then has to interpret.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("google-ads performance max creation aborted after campaign %s created (context done before asset group create): %w", campaignID, ctxErr)
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
