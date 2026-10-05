// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// WriteBudget implements service.BudgetWriter for Reddit: it changes the AMOUNT of the
// campaign's budget, after establishing that doing so moves this campaign's spend and nothing
// else's.
//
// WHERE THE BUDGET LIVES is decided by the create path, not assumed here. CreateCampaign sends
// is_campaign_budget_optimization=true with goal_type LIFETIME_SPEND and goal_value (micro-units)
// on the CAMPAIGN, and sends no budget on the ad group. So the object written is the campaign
// itself — no child id is involved — and the field written is goal_value alone.
//
// IT IS READ-THEN-WRITE like every sibling, and the read establishes three facts:
//
//   - THE BUDGET IS STILL ON THE CAMPAIGN (is_campaign_budget_optimization is true). With it off,
//     spend is governed per AD GROUP, and writing the campaign's goal would not change what the
//     caller thinks it changes. Meta's precedent decides the multi-ad-group case: an allocation
//     across ad groups is refused rather than guessed. This service creates no such campaign, so
//     only a campaign changed by hand in Reddit Ads Manager reaches that refusal. An UNREPORTED
//     flag is refused too — the same fail-closed reading Google applies to explicitly_shared.
//   - THE PACING ALREADY MATCHES. goal_type LIFETIME_SPEND is lifetime and DAILY_SPEND is daily;
//     a request naming the other is refused, never translated, exactly as on every sibling. Any
//     other or unreported goal_type is a shape this service has no mapping for.
//   - THE CURRENT AMOUNT IS LEGIBLE. A goal_value that is present but unreadable is refused
//     rather than read as "no budget".
//
// There is NO shared-budget analogue: a Reddit campaign's goal cannot be attached to another
// campaign, so that refusal is deliberately absent rather than forgotten.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, so a caller seeing any refusal below knows
// the platform is untouched — which is what lets the service answer them 409/400 rather than
// "verify upstream".
func (d *RedditDispatcher) WriteBudget(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, budget model.BudgetChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE.
	//
	// STRICTER than verifyRedditAccountMatch, which waves an absent creating account through as
	// "unknown, proceed" — the pre-existing-row case ToggleStatus and ReadMetrics tolerate, and
	// on Reddit that case has no recoverable fallback at all (see redditCreationAccountID). A
	// budget write may not tolerate it: a Reddit campaign id is unique only within its ad
	// account, and the cost of being wrong here is changing the budget of a campaign in another
	// account. The shared helper is still called BELOW for the mismatch wording.
	created := redditCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write reddit campaign budget: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	// The AMOUNT is validated before any credential is resolved or any call is made: it is a
	// pure function of the request, through the SAME bound and rounding the create path uses,
	// and a refusal is a permanent request fault (400), not an upstream one.
	micros, err := reddit.BudgetMicros(budget.Amount)
	if err != nil {
		if reason, ok := reddit.BudgetAmountReason(err); ok {
			return &rejectedBudgetAmountError{
				reason: reason,
				err:    fmt.Errorf("write reddit campaign budget: %w: %w", err, domain.ErrBudgetAmountRejected),
			}
		}
		return fmt.Errorf("write reddit campaign budget: %w", err)
	}

	// The SAME resolution ToggleStatus and ReadMetrics use: the connection for the account the
	// campaign was CREATED under, with every connection-state defect tagged (and system-scoped)
	// by resolveRedditClient.
	client, err := d.resolveRedditClient(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	if err := verifyRedditAccountMatch("write reddit campaign budget", campaign, client); err != nil {
		return err
	}

	current, err := client.GetCampaignBudget(ctx, campaign.PlatformCampaignID)
	if err != nil {
		// A PURE READ. Its failure is DEFINITE — no mutate was built — so it is returned
		// unclassified. An invalid id is a defect in the persisted row, not an upstream fault.
		if errors.Is(err, reddit.ErrInvalidCampaignID) || errors.Is(err, reddit.ErrInvalidAccountID) {
			return fmt.Errorf("write reddit campaign budget: %w: %w", err, domain.ErrBudgetUnwritable)
		}
		return fmt.Errorf("write reddit campaign budget: read current budget: %w", err)
	}
	if current == nil {
		return fmt.Errorf("%w: reddit campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}

	// GUARD 0 — THE CAMPAIGN MUST BE IN THE ACCOUNT THE CLIENT IS SCOPED TO. The path is already
	// account-scoped, so this only matters if Reddit resolves a campaign id regardless of the
	// path's account; when the answer names its account, it is checked rather than assumed.
	// An unreported account is accepted — the path scoping is the primary guard.
	if current.AdAccountID != "" && current.AdAccountID != strings.TrimSpace(client.AccountID()) {
		return fmt.Errorf("write reddit campaign budget: campaign %s is reported under ad account %s, not the connection's account %s: %w",
			campaign.PlatformCampaignID, current.AdAccountID, client.AccountID(), domain.ErrCampaignAccountMismatch)
	}

	// GUARD 1 — THE BUDGET MUST BE ON THE CAMPAIGN.
	if current.CampaignBudgetOptimization == nil {
		return fmt.Errorf("write reddit campaign budget: campaign %s did not report whether its budget is held on the campaign (is_campaign_budget_optimization), so the level that governs its spend cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}
	if !*current.CampaignBudgetOptimization {
		return fmt.Errorf("write reddit campaign budget: campaign %s has campaign budget optimization OFF, so its spend is governed by each ad group's own budget; this service will not choose how to allocate an amount across ad groups — change the budget in Reddit Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 2 — THE CURRENT AMOUNT MUST BE LEGIBLE.
	if current.GoalValueUnparseable {
		return fmt.Errorf("write reddit campaign budget: campaign %s reported a goal_value this service could not read: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 3 — THE PACING MODEL MUST ALREADY MATCH.
	var upstreamType model.BudgetType
	switch current.GoalType {
	case reddit.GoalTypeLifetimeSpend:
		upstreamType = model.BudgetLifetime
	case reddit.GoalTypeDailySpend:
		upstreamType = model.BudgetDaily
	default:
		reported := current.GoalType
		if reported == "" {
			reported = "no goal_type"
		}
		return fmt.Errorf("write reddit campaign budget: campaign %s reports %s, a budget shape this service has no mapping for: %w",
			campaign.PlatformCampaignID, reported, domain.ErrBudgetUnwritable)
	}
	if upstreamType != budget.Type {
		return fmt.Errorf("write reddit campaign budget: campaign %s is paced as %q upstream but the request asks for %q; this endpoint changes a budget's amount, never its pacing model — change the pacing in Reddit Ads Manager, then set the amount here: %w",
			campaign.PlatformCampaignID, upstreamType, budget.Type, domain.ErrBudgetUnwritable)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified: a timeout, 3xx, exhausted 429, 5xx, or a 2xx whose echo names
	// another campaign or amount may leave a new amount applied upstream. An unwrapped return
	// would be answered "the campaign was not modified" — a false claim about a money-moving
	// write. A definite 4xx is a refusal and passes through unwrapped.
	if err := client.UpdateCampaignBudget(ctx, campaign.PlatformCampaignID, micros); err != nil {
		werr := fmt.Errorf("write reddit campaign budget for campaign %s: %w", campaign.PlatformCampaignID, err)
		if reddit.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBudgetWriteError{err: werr}
		}
		return werr
	}
	return nil
}
