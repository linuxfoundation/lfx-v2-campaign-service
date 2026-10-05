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
// WHERE THE BUDGET LIVES is INFERRED from the create path and is unverified against a live
// account. CreateCampaign sends daily_budget_amount_local_micro on the CAMPAIGN, sends no
// budget_optimization, and sends no budget on the line item; X's v11 announcement makes CAMPAIGN
// the default and the only model in which a campaign-level daily budget is valid, so a campaign
// this service created is expected to report CAMPAIGN. X's current reference page instead lists
// LINE_ITEM as the only value and default. The writer therefore never relies on the inference:
// it writes only when the read REPORTS CAMPAIGN, and a campaign reporting LINE_ITEM or omitting
// the field is refused (409) before any write. The object written is the campaign itself — no
// line-item id is involved — through PUT accounts/:account_id/campaigns/:campaign_id
// (https://docs.x.com/x-ads-api/campaign-management/reference, "Campaigns").
//
// DAILY ONLY. A `lifetime` request is refused (409) before any call. Under CAMPAIGN budget
// optimization X requires the daily budget on the campaign (the same v11 announcement), so a
// CAMPAIGN campaign is always paced daily — a total_budget_amount_local_micro, where present, is
// an additional whole-flight cap, not an alternative pacing — and the create path never sets a
// total at all. No X document this service can cite shows a total-only campaign under CAMPAIGN,
// so supporting one would be supporting a shape the cited contract says cannot exist.
//
// IT IS READ-THEN-WRITE like every sibling, and the read establishes three facts:
//
//   - THE BUDGET IS ON THE CAMPAIGN (budget_optimization is CAMPAIGN). Under LINE_ITEM, X requires
//     the daily budget on each line item and forbids it on the campaign, so spend is governed per
//     line item and writing the campaign would not change what the caller thinks it changes.
//     This service is not expected to create such a campaign (an inference, unverified live —
//     if it does, every X budget write is refused, never misapplied). One is refused (409)
//     rather than allocated across line items, as Reddit refuses CBO-off and Meta refuses an
//     allocation across ad sets. An
//     UNREPORTED or unrecognised value is refused too — X's own documentation disagrees with
//     itself about the default (see twitter.BudgetOptimizationCampaign), which is precisely the
//     situation in which assuming would be guessing.
//   - THE CAMPAIGN IS DAILY-ONLY. Its daily cap must be set (the contract requires it under
//     CAMPAIGN; its absence — total-only or neither — contradicts the cited contract and is
//     refused rather than interpreted) and no total cap may be set: with both, rewriting the
//     daily amount under a `daily` label would describe only half of what governs its spend.
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

	// DAILY ONLY, decided from the REQUEST before anything is resolved or read (see the doc
	// comment): a CAMPAIGN-optimized X campaign is always paced daily, so a lifetime request is a
	// pacing this endpoint never changes, and nothing needs to be read to know that.
	switch budget.Type {
	case model.BudgetDaily:
	case model.BudgetLifetime:
		return fmt.Errorf("write x ads campaign budget: campaign %s: X campaign budgets are written only as a daily amount — under campaign budget optimization X requires a daily budget on the campaign, and this service's create path sets only that; a lifetime (total) budget is never changed here, so change it in X Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
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

	// GUARD 3 — THE CAMPAIGN MUST BE DAILY-ONLY.
	switch {
	case current.DailyMicros == nil:
		return fmt.Errorf("write x ads campaign budget: campaign %s reports no daily budget, which X requires on the campaign under campaign budget optimization, so its budget shape cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	case current.TotalMicros != nil:
		return fmt.Errorf("write x ads campaign budget: campaign %s carries a total budget cap as well as a daily one; this endpoint changes the daily amount of a daily-only campaign and never a pacing model — change the budget in X Ads Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified: a timeout, 3xx, exhausted 429, 5xx, a refusal after a retried
	// 429, or a 2xx whose echo names another campaign or amount may leave a new amount applied
	// upstream. An unwrapped return would be answered "the campaign was not modified" — a false
	// claim about a money-moving write. A definite 4xx is a refusal and passes through unwrapped.
	if err := client.UpdateCampaignBudget(ctx, campaign.PlatformCampaignID, micros); err != nil {
		werr := fmt.Errorf("write x ads campaign budget for campaign %s: %w", campaign.PlatformCampaignID, err)
		if twitter.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBudgetWriteError{err: werr}
		}
		return werr
	}
	return nil
}
