// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"errors"

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
// Each duplicate-name check is GATED BY THE ID STATE OF ITS OWN STEP, and that gate is
// load-bearing rather than defensive. parseErrorCodes keeps the bare enum VALUE and discards
// the error family, so hasErrorCode matches "DUPLICATE_NAME" raised by ANY mutate in the
// cascade, not only the budget's — and Performance Max keeps mutating long after the campaign
// exists (createPerformanceMaxAssetGroup, which calls assetGroups:mutate). Ungated, a duplicate
// name from one of those later steps would be reported as "nothing new was created" while a
// billable campaign was live. The ids are the better evidence everywhere except at the budget
// step itself, which is the one place they cannot distinguish a DUPLICATE_NAME refusal from an
// unconfirmed budget create — both leave the same id-less partial and mean opposite things.
func CreateOutcome(result *CampaignResult, err error) (string, bool) {
	if err == nil || result == nil {
		return "", false
	}

	switch {
	case result.CampaignBudgetID == "" && isDuplicateBudgetNameErr(err):
		// A definite 4xx (429 excluded) naming the budget-name code, and no budget id came
		// back: this attempt created nothing, and the name is held by a budget an earlier
		// attempt left behind.
		return domain.CreateOutcomeBudgetNameTaken, true
	case result.CampaignID == "" && result.CampaignBudgetID != "" && isDuplicateCampaignNameErr(err):
		// The budget id gate is what makes this token's sentence true: it promises the
		// budget for THIS attempt was created, which only a returned budget id establishes.
		return domain.CreateOutcomeCampaignNameTaken, true
	case result.CampaignID != "":
		// The campaign resource name came back, so the campaign exists whatever failed
		// after it.
		return domain.CreateOutcomeCampaignCreatedSetupIncomplete, true
	case result.CampaignBudgetID != "":
		// The budget's resource name came back and the campaign's did not. Claiming the
		// campaign was REFUSED needs positive evidence of a refusal, not merely the absence
		// of ambiguity: ten arms across the five channels hand back this partial with a
		// plain fmt.Errorf — a 2xx whose resource name would not parse or validate (past
		// which the campaign DOES exist), and the ctx.Err() arms between the two mutates,
		// where no campaign request was ever sent. Reading those as "refused" would tell the
		// operator to delete a budget whose campaign is live and billable, which is a worse
		// answer than the generic sentence this classifier replaced.
		//
		// The error codes must have PARSED for the same reason. This token's sentence says no
		// campaign of the composed name exists, and the arm above is what rules out the one
		// refusal that would make that false — DUPLICATE_CAMPAIGN_NAME, which means an earlier
		// attempt's campaign is still there. That arm can only rule it out from codes it could
		// read: doRequest builds an apiError with ErrorCodes nil when a non-2xx body is
		// unreadable or over maxResponseBytes (client.go), and a duplicate-name refusal
		// arriving that way would fall straight through to here. Requiring codes keeps the
		// claim resting on what was read rather than on what could not be.
		var ae *apiError
		if errors.As(err, &ae) && isDefiniteClientError(ae) && len(ae.ErrorCodes) > 0 {
			return domain.CreateOutcomeBudgetCreatedCampaignRefused, true
		}
		return domain.CreateOutcomeBudgetCreatedCampaignUnconfirmed, true
	}

	// A partial with no ids at all: the budget create was sent and its outcome is unknown
	// (ambiguous failure, or a 2xx whose resource name would not parse). Nothing downstream
	// was reached.
	return domain.CreateOutcomeUnconfirmed, true
}
