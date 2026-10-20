// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// CreateOutcome classifies what a failed campaign create left in the ad account, as one of
// domain's fixed create-outcome tokens. Mirrors BidAmountReason: the adapter answers the
// question in its own vocabulary, and the dispatcher carries the answer instead of a rendered
// error chain.
//
// It is deliberately ONE function reading the partial RESULT rather than an annotation on each
// of the create paths' ~50 partial-returning arms. The five create entrypoints
// (CreateCampaign, CreateDemandGenCampaign, CreateDisplayCampaign,
// CreatePerformanceMaxCampaign, CreateVideoCampaign) all build their partials by the same
// escalating contract — the pre-budget partial carries names only, the post-budget partial adds
// CampaignBudgetID, the post-campaign partial adds CampaignID — so the ids a partial carries
// already ARE the account state, and reading them classifies every arm of every channel at
// once. Per-arm tokens would restate that contract ~50 times and drift from it one arm at a
// time.
//
// Returns ("", false) for anything that is not a partial-result failure: a nil error (the
// create succeeded) or a nil result (the create failed before anything was sent, which the
// caller already reports as a pre-create failure, claim released).
//
// The duplicate-name checks run FIRST and in this order, because they are the two cases the
// ids alone cannot distinguish: a DUPLICATE_NAME refusal of the budget leaves the SAME
// id-less partial as an unconfirmed budget create, and means something quite different.
func CreateOutcome(result *CampaignResult, err error) (string, bool) {
	if err == nil || result == nil {
		return "", false
	}

	switch {
	case isDuplicateBudgetNameErr(err):
		// A definite 4xx (429 excluded) naming the budget-name code: this attempt created
		// nothing, and the name is held by a budget an earlier attempt left behind.
		return domain.CreateOutcomeBudgetNameTaken, true
	case isDuplicateCampaignNameErr(err):
		return domain.CreateOutcomeCampaignNameTaken, true
	case result.CampaignID != "":
		// The campaign resource name came back, so the campaign exists whatever failed
		// after it.
		return domain.CreateOutcomeCampaignCreatedSetupIncomplete, true
	case result.CampaignBudgetID != "":
		// The budget's resource name came back and the campaign's did not. Whether a
		// campaign exists anyway is exactly the ambiguity createOutcomeAmbiguous answers:
		// a 5xx/429/3xx/transport failure may still have committed the campaign create,
		// while a definite 4xx refused it.
		if createOutcomeAmbiguous(err) {
			return domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true
		}
		return domain.CreateOutcomeBudgetCreatedCampaignRefused, true
	}

	// A partial with no ids at all: the budget create was sent and its outcome is unknown
	// (ambiguous failure, or a 2xx whose resource name would not parse). Nothing downstream
	// was reached.
	return domain.CreateOutcomeUnconfirmed, true
}
