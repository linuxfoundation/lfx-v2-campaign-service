// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// microsoftCampaignIDRE is the shape of a Microsoft campaign id: a positive integer. The client
// enforces the same rule; it is checked here as well so a corrupt row is refused as a fact about
// the row (409) before any credential is decrypted, rather than as a client error (503).
var microsoftCampaignIDRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// WriteBudget implements service.BudgetWriter for Microsoft Advertising: it changes the campaign's
// DAILY budget amount, after establishing that doing so moves this campaign's spend and nothing
// else's.
//
// IT IS READ-THEN-WRITE like every sibling, and Microsoft's model sits between LinkedIn's and
// Google's: the budget is a pair of FIELDS ON THE CAMPAIGN (DailyBudget, BudgetType) — so a
// campaign id addresses it and there is no budget-id resolution — UNLESS the campaign is attached
// to a SHARED Budget entity (BudgetId), in which case those fields are read-only echoes of a pool
// other campaigns also draw on. So the shared-budget guard Google has, and LinkedIn deliberately
// lacks, reappears here, reached through BudgetId.
//
// DAILY STANDARD ONLY. Microsoft's BudgetLimitType reference documents both
// DailyBudgetAccelerated and LifetimeBudgetStandard as available ONLY to Audience campaigns
// (https://learn.microsoft.com/en-us/advertising/campaign-management-service/budgetlimittype),
// and this service creates and reads Search campaigns only — so DailyBudgetStandard is the one
// pacing a campaign this path can reach may have. A lifetime request is therefore the
// pacing-mismatch refusal every sibling makes — ErrBudgetUnwritable, 409 — raised here before
// anything is resolved, because no Microsoft campaign this path can reach is paced that way.
//
// Amounts are a plain decimal in the AD ACCOUNT's currency — not micros, not minor units — and
// are sent unrounded; see microsoft.ValidateDailyBudget for why, and for what decides the
// smallest settable amount instead.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, so a caller seeing any refusal below knows the
// platform is untouched — which is what lets the service answer them 409 rather than "verify
// upstream". The PUT's own DEFINITE refusals (a shared budget attached since the read, an amount
// Microsoft rejects) are sent-and-refused rather than refused-before-sending, but carry the same
// guarantee: Microsoft confirmed nothing was applied. An UNCONFIRMED outcome is never one of them.
func (d *MicrosoftDispatcher) WriteBudget(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, budget model.BudgetChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE.
	//
	// This guard is STRICTER than verifyMicrosoftAccountMatch, and the difference is the point.
	// That helper returns nil when the row records no creating account — "unknown, proceed",
	// which ToggleStatus and ReadMetrics deliberately tolerate for rows written before the
	// account id was stamped. A budget write may not: BudgetWriter requires the account-identity
	// invariant to be enforced AT LEAST AS STRICTLY as the read path, and the cost of being
	// wrong here is changing the budget of a campaign in another account. Microsoft campaign ids
	// are unique only within an account, so an unrecorded account is exactly the case in which
	// the id cannot be trusted to name this campaign.
	//
	// The absence is refused HERE, and the shared helper is still called BELOW for the mismatch
	// case, so the wording stays common across every Microsoft path.
	created := microsoftCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write microsoft campaign budget: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	// PACING — refused locally, for the reason in the doc comment: there is no Microsoft
	// campaign this path can reach whose upstream pacing is a lifetime budget, so the request
	// necessarily names a pacing the campaign does not have. Same sentinel, same 409, as the
	// siblings' pacing-mismatch refusal.
	if budget.Type != model.BudgetDaily {
		return fmt.Errorf("write microsoft campaign budget: campaign %s is a Microsoft Advertising Search campaign, which is paced by a DAILY budget only, but the request asks for %q; this endpoint changes a budget's amount, never its pacing model: %w",
			campaign.PlatformCampaignID, budget.Type, domain.ErrBudgetUnwritable)
	}

	// ID SHAPE — a fact about the row. A non-numeric id addresses nothing Microsoft could hold,
	// and is refused before a credential is decrypted rather than surfacing as a client error.
	campaignID := strings.TrimSpace(campaign.PlatformCampaignID)
	if !microsoftCampaignIDRE.MatchString(campaignID) {
		return fmt.Errorf("write microsoft campaign budget: the recorded platform campaign id %q is not a Microsoft campaign id, so there is nothing to address: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// The SAME resolution and the SAME guard ToggleStatus uses: resolveExisting follows the
	// account the campaign was CREATED under, and verifyMicrosoftAccountMatch refuses a
	// connection re-pointed elsewhere since — both before Microsoft is contacted.
	client, err := d.resolveMicrosoftClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	if err := verifyMicrosoftAccountMatch("write microsoft campaign budget", campaign, client); err != nil {
		return err
	}

	current, err := client.GetCampaignBudget(ctx, campaignID)
	if err != nil {
		// A PURE READ. Its failure is DEFINITE — no mutate was built — so it is returned
		// unclassified. Marking it unconfirmed would send an operator to verify a write that
		// never existed.
		return fmt.Errorf("write microsoft campaign budget: read current budget: %w", err)
	}
	if current == nil {
		return fmt.Errorf("%w: microsoft campaign %s", domain.ErrPlatformCampaignAbsent, campaignID)
	}

	// GUARD 1 — AN EXPERIMENT CAMPAIGN'S BUDGET IS NOT ITS OWN. Microsoft documents that an
	// experiment campaign's budget is inherited from its base campaign and cannot be set; the
	// change belongs on the base campaign, where its split across the experiment is visible.
	if current.ExperimentID != "" {
		return fmt.Errorf("write microsoft campaign budget: campaign %s is a Microsoft Advertising experiment campaign, whose budget is inherited from its base campaign and cannot be set on it; change the base campaign's budget in Microsoft Advertising: %w",
			campaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 2 — SHARED BUDGET. A campaign with a BudgetId draws on a pool other campaigns in the
	// account also draw on, so changing it through this campaign would change their spend too —
	// including campaigns this service does not own and the caller cannot see. An UNREADABLE
	// BudgetId is refused too, never assumed private: "we could not establish that this budget
	// is private" and "this budget is private" are opposite facts, and only the second justifies
	// a write that moves money.
	if current.BudgetIDUnreadable {
		return fmt.Errorf("write microsoft campaign budget: campaign %s reported a BudgetId this service could not read, so whether its budget is shared cannot be established: %w",
			campaignID, domain.ErrBudgetUnwritable)
	}
	if current.IsShared() {
		return fmt.Errorf("write microsoft campaign budget: campaign %s is attached to a SHARED budget, so changing its amount would change every other campaign drawing on that budget — including campaigns this request did not name; give the campaign its own budget in Microsoft Advertising, or make the change there where its full effect is visible: %w",
			campaignID, domain.ErrBudgetShared)
	}

	// GUARD 3 — THE PACING MUST BE DailyBudgetStandard, AS MICROSOFT REPORTED IT. The request is
	// already known to be daily; this establishes that the campaign is too. The read asks for
	// CampaignType Search explicitly, and Microsoft documents DailyBudgetAccelerated as available
	// ONLY to Audience campaigns with unshared campaign-level budgets, so a Search campaign
	// reporting it is a response that contradicts its own documentation. It is refused rather than
	// echoed back on the PUT: a write resting on a read this service cannot explain is a guard in
	// name only. An unreported type is refused rather than defaulted for the same reason.
	switch current.BudgetType {
	case microsoft.BudgetTypeDailyStandard:
	case "":
		return fmt.Errorf("write microsoft campaign budget: campaign %s did not report its budget type, so the write cannot preserve it: %w",
			campaignID, domain.ErrBudgetUnwritable)
	case microsoft.BudgetTypeDailyAccelerated:
		return fmt.Errorf("write microsoft campaign budget: campaign %s was read as a Search campaign but reports budget type %q, which Microsoft Advertising documents as available only to Audience campaigns; this service does not write a budget whose reported pacing contradicts the campaign's type — check the campaign in Microsoft Advertising: %w",
			campaignID, current.BudgetType, domain.ErrBudgetUnwritable)
	case microsoft.BudgetTypeLifetimeStandard:
		return fmt.Errorf("write microsoft campaign budget: campaign %s is paced as %q upstream but the request asks for %q; this endpoint changes a budget's amount, never its pacing model — change the pacing in Microsoft Advertising, then set the amount here: %w",
			campaignID, model.BudgetLifetime, budget.Type, domain.ErrBudgetUnwritable)
	default:
		return fmt.Errorf("write microsoft campaign budget: campaign %s reports budget type %q, which this service has no mapping for: %w",
			campaignID, current.BudgetType, domain.ErrBudgetUnwritable)
	}

	// The same bounds the create path applies. The service layer has already enforced finite,
	// > 0 and <= its own ceiling (the same 1e9), so this cannot fail today; it is mapped anyway
	// so a future divergence answers 400 rather than the default 503.
	if verr := microsoft.ValidateDailyBudget(budget.Amount); verr != nil {
		return microsoftBudgetAmountRejected(verr)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified, unlike the read above: a timeout, 3xx, exhausted 429 or 5xx
	// on the PUT may leave the new amount applied upstream. An unwrapped return here would reach
	// the service as a definite failure and be answered "the campaign was not modified" — an
	// affirmative false claim about a money-moving write.
	if err := client.UpdateCampaignDailyBudget(ctx, campaignID, budget.Amount, current.BudgetType); err != nil {
		werr := fmt.Errorf("write microsoft campaign budget for campaign %s: %w", campaignID, err)
		switch {
		case microsoft.IsOutcomeUnconfirmed(err):
			return &unconfirmedBudgetWriteError{err: werr}
		case errors.Is(err, microsoft.ErrSharedBudget):
			// Microsoft's own backstop for a budget attached between the read and the write.
			// The PUT WAS sent, but this is a DEFINITE refusal — Microsoft confirmed nothing was
			// applied — so it is the same "platform unchanged" 409 as the guard above. That, not
			// "refused before any mutate", is what ErrBudgetShared promises.
			return fmt.Errorf("%w: %w", werr, domain.ErrBudgetShared)
		case errors.Is(err, microsoft.ErrBudgetAmountInvalid):
			// Likewise sent and DEFINITELY refused: Microsoft rejected the amount and applied
			// nothing, so the 400 still means "platform unchanged".
			return microsoftBudgetAmountRejected(err)
		}
		return werr
	}
	return nil
}

// microsoftBudgetAmountRejected maps a Microsoft amount refusal to the service's 400, carrying
// only the client's own sentence as the reason.
func microsoftBudgetAmountRejected(err error) error {
	reason, ok := microsoft.BudgetAmountReason(err)
	if !ok {
		reason = "the requested daily budget is not accepted by Microsoft Advertising"
	}
	return &rejectedBudgetAmountError{
		reason: reason,
		err:    fmt.Errorf("write microsoft campaign budget: %w: %w", err, domain.ErrBudgetAmountRejected),
	}
}
