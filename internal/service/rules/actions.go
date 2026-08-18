// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Priority is how urgently an action item wants attention.
type Priority string

const (
	PriorityHigh   Priority = "HIGH"
	PriorityMedium Priority = "MED"
)

// ActionItem is one thing an operator should look at, and what to do about it.
type ActionItem struct {
	// Rule identifies WHICH rule fired, as a stable token. The Issue text is for humans and
	// may be reworded; a consumer grouping or filtering by rule must not key on prose.
	Rule     string
	Priority Priority
	// CampaignID is the campaign this concerns, so a consumer can link to it without
	// matching on the name.
	CampaignID string
	Platform   string
	Issue      string
	Action     string
}

// Input is one campaign's measured state, as the rules need to see it.
//
// Deliberately NOT the wire type: the rules take plain numbers so they can be tested without
// constructing a metrics response, and so a change to the API shape does not silently change
// what the rules mean.
type Input struct {
	CampaignID string
	Platform   string
	// Status is the campaign's own lifecycle value as this service records it, not the
	// platform's run state — nothing here reads the platform's ENABLED/PAUSED back.
	Status      string
	Impressions int64
	Clicks      int64
	// Spend is the campaign's cost over the measured window, in whole units of the PLATFORM's
	// own currency. Deliberately not named SpendUSD: this service performs no FX conversion, so
	// the unit is whatever the platform bills in. It is only ever compared against that same
	// campaign's budget, and must never be summed or averaged across campaigns.
	Spend float64
	// CTRPct is clicks/impressions as a PERCENTAGE, matching what the UI renders. A ratio
	// would silently make every low-CTR threshold a hundred times too strict.
	CTRPct float64
	Pacing Pacing
	// BillsPerDelivery records whether this channel charges for delivery at all.
	//
	// False for the email channel: HubSpot charges nothing per send and its adapter always
	// reports CostMicros=0, so "no spend" carries no information there. It also maps opens onto
	// Impressions, so a delivered email nobody opened looks identical to a campaign that never
	// ran — and zero_delivery would tell an operator to check targeting and approval for an
	// email that was delivered exactly as intended.
	BillsPerDelivery bool
}

// LowCTRThresholdPct is the floor below which click-through is worth flagging.
//
// One value for every platform. The four UI implementations used 0.3, 0.3 and 0.5 with no
// stated reason for the difference, which is drift rather than a platform characteristic — if a
// platform genuinely runs lower, that is an override with a comment, not a silent divergence.
const LowCTRThresholdPct = 0.3

// Evaluate runs every rule against one campaign and returns what fired, in a stable order.
//
// Rules never fire on a campaign whose metrics could not be READ — that is the caller's job to
// filter, because "we could not measure this" and "this measured zero" want opposite responses
// and only the caller knows which it has.
func Evaluate(in Input) []ActionItem {
	var items []ActionItem

	// Zero delivery. An active campaign with no impressions AND no spend is not underspending
	// — it is not running at all, and no budget change will fix it.
	//
	// Both conditions are required: impressions without spend is an unbilled serve, and spend
	// without impressions is a billing artefact. Either alone is noise; together they mean the
	// campaign never started.
	// BillsPerDelivery gates the whole rule: on a channel that charges nothing per delivery,
	// "no spend" is the normal state and says nothing about whether the campaign ran.
	zeroDelivery := in.BillsPerDelivery && isActive(in.Status) && in.Impressions == 0 && in.Spend == 0
	if zeroDelivery {
		items = append(items, ActionItem{
			Rule: "zero_delivery", Priority: PriorityHigh,
			CampaignID: in.CampaignID, Platform: in.Platform,
			Issue:  "Campaign is active but has not delivered — no impressions and no spend",
			Action: "Check the campaign is approved and its targeting is not so narrow that it cannot serve",
		})
	}

	// Pacing. Only when a figure could actually be derived: an incomputable pacing carries
	// Pct=0, and firing "underspending at 0%" on a campaign with no recorded budget would be
	// reporting the absence of data as a finding about spend.
	//
	// Suppressed entirely when zero_delivery fired. A campaign that never started is trivially
	// at 0% of plan, so the pacing item is arithmetically true and actively misleading: the two
	// items carry OPPOSITE remedies — zero_delivery says no budget change will fix this, while
	// underspending says to adjust the budget. Emitting both makes the operator pick.
	if in.Pacing.Computable && !zeroDelivery {
		switch in.Pacing.Label {
		case PacingUnderspending:
			items = append(items, ActionItem{
				Rule: "underspending", Priority: PriorityHigh,
				CampaignID: in.CampaignID, Platform: in.Platform,
				Issue:  fmt.Sprintf("Underspending — %.0f%% of expected spend for this point in the flight", in.Pacing.Pct),
				Action: "Broaden targeting, raise the bid, or reduce the budget to match realistic delivery",
			})
		case PacingConstrained, PacingOverspending:
			items = append(items, ActionItem{
				Rule: "budget_constrained", Priority: PriorityMedium,
				CampaignID: in.CampaignID, Platform: in.Platform,
				Issue:  fmt.Sprintf("Spending ahead of plan — %.0f%% of expected spend for this point in the flight", in.Pacing.Pct),
				Action: "Raise the budget if the campaign is worth more delivery, or accept it will exhaust early",
			})
		}
	}

	// Low CTR. Guarded on impressions rather than on CTR alone: a campaign with three
	// impressions and no clicks has a 0% CTR that means nothing, and flagging it trains
	// operators to ignore the rule.
	if in.Impressions >= minImpressionsForCTR && in.CTRPct < LowCTRThresholdPct {
		items = append(items, ActionItem{
			Rule: "low_ctr", Priority: PriorityMedium,
			CampaignID: in.CampaignID, Platform: in.Platform,
			Issue:  fmt.Sprintf("Low click-through — %.2f%% across %d impressions", in.CTRPct, in.Impressions),
			Action: "Refresh the ad copy or creative, and check the audience is the one the message is written for",
		})
	}

	return items
}

// minImpressionsForCTR is the delivery below which a CTR figure is not yet meaningful.
//
// The UI implementations had no such floor, so a campaign with a handful of impressions and no
// clicks produced a "low CTR" item on its first hour. Stated as a named constant rather than an
// inline literal so the next person changing it knows it is a judgement, not a platform limit.
const minImpressionsForCTR = 1000

// isActive reports whether the campaign is in a state where delivery is expected.
//
// An ALLOW-LIST of this service's own lifecycle values, not an exclusion. The vocabulary is
// `pending`, `created`, `created_degraded`, `group_created`, `unconfirmed` and `deleted`
// (model.CampaignStatus*), and only two of those describe something that exists upstream and
// should be serving:
//
//   - created — the campaign was created and confirmed.
//   - created_degraded — the campaign was created, but some variants failed. It IS delivering,
//     so a zero-delivery finding on it is real; excluding it would hide a broken campaign
//     precisely because it was already partly broken.
//
// The rest are deliberately excluded. `pending` is an in-flight claim that may not have reached
// the platform at all; `unconfirmed` and `group_created` are partial states where a
// zero-delivery finding would report a dispatch problem as a targeting one; `deleted` is gone.
//
// Written as an allow-list because `status != deleted` would sweep every future status into
// "expected to deliver" the moment one is added, with nothing failing to say so.
func isActive(status string) bool {
	return status == model.CampaignStatusCreated || status == model.CampaignStatusCreatedDegraded
}
