// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// WriteBudget implements service.BudgetWriter for X (Twitter) Ads: it changes the AMOUNT of the
// campaign's budget, after establishing that doing so moves this campaign's spend and nothing
// else's.
//
// WHERE THE BUDGET LIVES is decided by the create path, not assumed here. CreateCampaign sends
// daily_budget_amount_local_micro on the CAMPAIGN, sends no budget_optimization (X's v11 default
// is CAMPAIGN, campaign budget optimization), and sends no budget on the line item. So the object
// written is the campaign itself — no line-item id is involved — through
// PUT accounts/:account_id/campaigns/:campaign_id
// (https://docs.x.com/x-ads-api/campaign-management/reference, "Campaigns").
//
// IT IS READ-THEN-WRITE like every sibling, and the read establishes three facts:
//
//   - THE BUDGET IS ON THE CAMPAIGN (budget_optimization is CAMPAIGN). Under LINE_ITEM, X requires
//     the daily budget on each line item and forbids it on the campaign, so spend is governed per
//     line item and writing the campaign would not change what the caller thinks it changes.
//     This service creates no such campaign; one is refused (409) rather than allocated across
//     line items, as Reddit refuses CBO-off and Meta refuses an allocation across ad sets. An
//     UNREPORTED or unrecognised value is refused too — X's own documentation disagrees with
//     itself about the default (see twitter.BudgetOptimizationCampaign), which is precisely the
//     situation in which assuming would be guessing.
//   - THE PACING ALREADY MATCHES. An X campaign carries a daily cap, a whole-flight cap, or both.
//     Only the daily cap set reads as daily (the shape CreateCampaign produces); only the total
//     set reads as lifetime. A request naming the other is refused, never translated, exactly as
//     on every sibling. A campaign with BOTH caps, or neither, has no single pacing this
//     endpoint's daily/lifetime vocabulary can name, so it is refused rather than having one of
//     its two caps rewritten under a label that describes only half of what governs its spend.
//   - THE CURRENT AMOUNTS ARE LEGIBLE. An amount reported but unreadable is refused rather than
//     read as "not set".
//
// There is NO shared-budget analogue: an X campaign's budget is a pair of fields on the campaign,
// not a resource another campaign can be attached to, so that refusal is deliberately absent.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, so a caller seeing any refusal below knows the
// platform is untouched — which is what lets the service answer them 409/400 rather than "verify
// upstream".
//
// GATING. This is a live-money write, and it is not behind a flag: X campaign WRITES (create,
// pause/activate) are already ungated in this service, and the budget PUT goes through the same
// signed client, the same write pacer and the same outcome classification. TWITTER_METRICS_ENABLED
// gates only the report-backed account monitor, whose stats-jobs contract is the unverified one.
func (d *TwitterDispatcher) WriteBudget(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, budget model.BudgetChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE.
	//
	// STRICTER than verifyTwitterAccountMatch, which waves an absent creating account through as
	// "unknown, proceed" — the pre-existing-row case ToggleStatus and ReadMetrics tolerate, and on
	// X that case has no recoverable fallback at all (see twitterCreationAccountID). A budget write
	// may not tolerate it: an X campaign id is unique only within its ad account, and the cost of
	// being wrong here is changing the budget of a campaign in another account. The shared helper
	// is still called BELOW for the mismatch wording.
	created := twitterCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write x ads campaign budget: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	// The AMOUNT is validated before any credential is resolved or any call is made: it is a pure
	// function of the request, through the SAME bound and rounding the create path uses, and a
	// refusal is a permanent request fault (400), not an upstream one.
	micros, err := twitter.BudgetMicros(budget.Amount)
	if err != nil {
		if reason, ok := twitter.BudgetAmountReason(err); ok {
			return &rejectedBudgetAmountError{
				reason: reason,
				err:    fmt.Errorf("write x ads campaign budget: %w: %w", err, domain.ErrBudgetAmountRejected),
			}
		}
		return fmt.Errorf("write x ads campaign budget: %w", err)
	}

	// The field is chosen from the REQUEST before anything is read, so an unknown type is refused
	// without a call. Which field the campaign actually paces on is checked against the read below.
	var field twitter.BudgetField
	switch budget.Type {
	case model.BudgetDaily:
		field = twitter.BudgetFieldDaily
	case model.BudgetLifetime:
		field = twitter.BudgetFieldTotal
	default:
		return fmt.Errorf("write x ads campaign budget: unsupported budget type %q: %w", budget.Type, domain.ErrBudgetUnwritable)
	}

	// The SAME resolution ToggleStatus and ReadMetrics use: the connection for the account the
	// campaign was CREATED under, validated by validateTwitterConnection with every defect tagged
	// (and system-scoped), and the SHARED cached client — so this PUT queues on the same write
	// pacer as any concurrent create or toggle on the account.
	client, res, err := d.resolveTwitterClientWithRes(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	if err := verifyTwitterAccountMatch("write x ads campaign budget", campaign, client); err != nil {
		return err
	}

	current, err := client.GetCampaignBudget(ctx, campaign.PlatformCampaignID)
	if err != nil {
		// A PURE READ. Its failure is DEFINITE — no mutate was built — so it is returned
		// unclassified, except for the two ids the client refuses before building any request,
		// whose owners differ and so whose remedies differ (see the Reddit writer, which this
		// follows): the ACCOUNT id is the connection's, a stored-connection defect; the CAMPAIGN
		// id is the persisted row's, a budget that cannot be addressed.
		if errors.Is(err, twitter.ErrInvalidAccountID) {
			return res.systemScoped(fmt.Errorf("%w: %w: write x ads campaign budget: the connection's ad account id cannot address an x ads request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, err))
		}
		if errors.Is(err, twitter.ErrInvalidCampaignID) {
			return fmt.Errorf("write x ads campaign budget: the campaign row's platform campaign id cannot address an x ads request: %w: %w", err, domain.ErrBudgetUnwritable)
		}
		return fmt.Errorf("write x ads campaign budget: read current budget: %w", err)
	}
	if current == nil {
		return fmt.Errorf("%w: x ads campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}

	// GUARD 1 — THE BUDGET MUST BE ON THE CAMPAIGN.
	switch current.BudgetOptimization {
	case twitter.BudgetOptimizationCampaign:
	case "":
		return fmt.Errorf("write x ads campaign budget: campaign %s did not report its budget_optimization, so the level that governs its spend cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	case twitter.BudgetOptimizationLineItem:
		return fmt.Errorf("write x ads campaign budget: campaign %s uses line-item budget optimization, so its spend is governed by each line item's own budget; this service will not choose how to allocate an amount across line items — change the budget in X Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	default:
		return fmt.Errorf("write x ads campaign budget: campaign %s reports budget_optimization %q, a budget model this service has no mapping for: %w",
			campaign.PlatformCampaignID, current.BudgetOptimization, domain.ErrBudgetUnwritable)
	}

	// GUARD 2 — THE CURRENT AMOUNTS MUST BE LEGIBLE.
	if current.DailyUnparseable || current.TotalUnparseable {
		return fmt.Errorf("write x ads campaign budget: campaign %s reported a budget amount this service could not read: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 3 — THE PACING MODEL MUST ALREADY MATCH.
	var upstreamType model.BudgetType
	switch {
	case current.DailyMicros != nil && current.TotalMicros == nil:
		upstreamType = model.BudgetDaily
	case current.DailyMicros == nil && current.TotalMicros != nil:
		upstreamType = model.BudgetLifetime
	case current.DailyMicros != nil && current.TotalMicros != nil:
		return fmt.Errorf("write x ads campaign budget: campaign %s carries both a daily and a total budget cap, a pacing this endpoint's daily/lifetime vocabulary cannot name — change the budget in X Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	default:
		return fmt.Errorf("write x ads campaign budget: campaign %s reports neither a daily nor a total budget, so it has no amount this endpoint can change: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}
	if upstreamType != budget.Type {
		return fmt.Errorf("write x ads campaign budget: campaign %s is paced as %q upstream but the request asks for %q; this endpoint changes a budget's amount, never its pacing model — change the pacing in X Ads Manager, then set the amount here: %w",
			campaign.PlatformCampaignID, upstreamType, budget.Type, domain.ErrBudgetUnwritable)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified: a timeout, 3xx, exhausted 429, 5xx, a refusal after a retried
	// 429, or a 2xx whose echo names another campaign or amount may leave a new amount applied
	// upstream. An unwrapped return would be answered "the campaign was not modified" — a false
	// claim about a money-moving write. A definite 4xx is a refusal and passes through unwrapped.
	if err := client.UpdateCampaignBudget(ctx, campaign.PlatformCampaignID, field, micros); err != nil {
		werr := fmt.Errorf("write x ads campaign budget for campaign %s: %w", campaign.PlatformCampaignID, err)
		if twitter.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBudgetWriteError{err: werr}
		}
		return werr
	}
	return nil
}
