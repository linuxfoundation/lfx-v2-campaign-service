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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// WriteBudget implements service.BudgetWriter for LinkedIn: it changes the AMOUNT of the
// campaign's budget, after establishing that doing so moves this campaign's spend and nothing
// else's.
//
// IT IS READ-THEN-WRITE for the same reason the Google Ads implementation is, but the facts the
// read establishes are DIFFERENT, and the difference is worth stating because it changes which
// guards exist here:
//
//   - LinkedIn has NO SEPARATE BUDGET RESOURCE. dailyBudget and totalBudget are fields on the
//     campaign itself, so a campaign id fully addresses its budget and there is no equivalent
//     of Google's budget-id resolution. There is also NOTHING TO SHARE: a LinkedIn budget
//     cannot be attached to a second campaign, so the shared-budget refusal that dominates the
//     Google path has no analogue and is deliberately absent rather than forgotten. Meta is
//     the platform where that guard reappears, in the CBO form.
//   - The two amount fields ARE still mutually exclusive in practice, and which one a campaign
//     uses is the pacing model. So the pacing-match guard below is the same guard Google makes,
//     reached through a different fact.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, so a caller seeing any refusal below knows
// the platform is untouched — which is what lets the service answer them 409 rather than
// "verify upstream".
func (d *LinkedInDispatcher) WriteBudget(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, budget model.BudgetChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE.
	//
	// This guard is STRICTER than verifyLinkedInAccountMatch, and the difference is the
	// point. That helper returns nil when the row records no creating account at all — the
	// pre-existing-row case, which ToggleStatus and ReadMetrics both tolerate. A budget
	// write may not: BudgetWriter's contract requires the account-identity invariant to be
	// enforced AT LEAST AS STRICTLY as the read path enforces it, and the cost of being
	// wrong here is changing the budget of a campaign in another account rather than
	// returning a misleading report.
	//
	// So the absence is refused HERE, and the shared helper is still called BELOW to answer
	// the mismatch case in the one wording every adapter uses. Calling the helper alone
	// would silently inherit a contract this path cannot accept — which is exactly how a
	// reused helper reopens the bug it was written to prevent.
	created := linkedInCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write linkedin campaign budget: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(res.accountID)
	client := d.cachedLinkedInClient(projectID, platform, res, creds, accountID)
	if err := verifyLinkedInAccountMatch("write linkedin campaign budget", campaign, accountID); err != nil {
		return err
	}

	current, err := client.GetCampaignBudget(ctx, campaign.PlatformCampaignID)
	if err != nil {
		// A PURE READ. Its failure is DEFINITE — no mutate was built — so it is returned
		// unclassified. Marking it unconfirmed would send an operator to verify a write
		// that never existed, which is the same mis-scoping the Google slice corrected.
		return fmt.Errorf("write linkedin campaign budget: read current budget: %w", err)
	}
	if current == nil {
		return fmt.Errorf("%w: linkedin campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}

	// GUARD 1 — THE CURRENT BUDGET MUST BE LEGIBLE. A field the platform reported but this
	// client could not parse is refused, not treated as absent. Every guard below reasons
	// about which budget field the campaign uses; reasoning about a value that failed to
	// parse is reasoning about zero, and the answer it produces would select the wrong
	// field to write.
	if current.AmountUnparseable {
		return fmt.Errorf("write linkedin campaign budget: campaign %s reported a budget amount this service could not read, so which of its budget fields is in use cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 2 — CURRENCY. The minimums enforced below ($10 daily, $100 lifetime) are
	// USD-specific, and this client only ever SENDS currencyCode "USD". A campaign
	// denominated in anything else is one whose minimums this service cannot check and
	// whose existing amount it would silently redenominate by writing USD over it. Refuse
	// rather than write a number in a currency nobody chose.
	//
	// An EMPTY currency is permitted: it means the platform did not report the field, which
	// is the shape a campaign with no budget set yet returns, and the write below supplies
	// USD explicitly. Only a reported, non-USD currency is a refusal.
	if current.CurrencyCode != "" && !strings.EqualFold(current.CurrencyCode, "USD") {
		return fmt.Errorf("write linkedin campaign budget: campaign %s is denominated in %s, but this service writes and validates LinkedIn budgets in USD only; change the amount in LinkedIn Campaign Manager where its currency is applied: %w",
			campaign.PlatformCampaignID, current.CurrencyCode, domain.ErrBudgetUnwritable)
	}

	// GUARD 3 — THE PACING MODEL MUST ALREADY MATCH. This capability changes HOW MUCH a
	// campaign may spend, never HOW it is paced — the same refusal the Google Ads path
	// makes, and for the same reason: switching a live campaign between daily pacing and a
	// whole-flight cap reinterprets every figure it has already spent against, and doing it
	// silently as a side effect of an amount change is not a thing to do without an
	// operator seeing it stated.
	//
	// LinkedIn reports the pacing model by WHICH FIELD IS PRESENT. A campaign reporting
	// NEITHER has no budget this service can identify the shape of, and a campaign
	// reporting BOTH is the genuinely ambiguous row — writing one of them would leave the
	// other in force and the effective budget would be neither what was asked for nor what
	// was there. Both are refused.
	hasDaily := current.DailyBudget != nil
	hasTotal := current.TotalBudget != nil
	switch {
	case hasDaily && hasTotal:
		return fmt.Errorf("write linkedin campaign budget: campaign %s reports BOTH a daily and a total budget, so writing either would leave the other in force; resolve which one applies in LinkedIn Campaign Manager: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	case !hasDaily && !hasTotal:
		return fmt.Errorf("write linkedin campaign budget: campaign %s reports no budget at all, so which of the two mutually exclusive amount fields to write cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}
	upstreamType := model.BudgetDaily
	if hasTotal {
		upstreamType = model.BudgetLifetime
	}
	if upstreamType != budget.Type {
		return fmt.Errorf("write linkedin campaign budget: campaign %s is paced as %q upstream but the request asks for %q; this endpoint changes a budget's amount, never its pacing model — change the pacing in LinkedIn Campaign Manager, then set the amount here: %w",
			campaign.PlatformCampaignID, upstreamType, budget.Type, domain.ErrBudgetUnwritable)
	}

	// Converted and validated through the SAME helper the create path uses, so an amount
	// this service would refuse to create with cannot be reached by editing. The WIRE
	// STRING it returns is what is sent — the float is never re-formatted downstream —
	// so the amount validated is byte-for-byte the amount written.
	lifetime := upstreamType == model.BudgetLifetime
	wire, _, err := linkedin.ValidateBudgetAmount(budget.Amount, lifetime)
	if err != nil {
		return fmt.Errorf("write linkedin campaign budget: %w", err)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified, unlike the read above: a timeout, 3xx, 429 or 5xx on the
	// PARTIAL_UPDATE may leave the new amount applied upstream. An unwrapped return here would
	// reach the service as a definite failure and be answered "the campaign was not modified" —
	// an affirmative false claim about a money-moving write, with the claim lock released
	// inline instead of held through the cooldown.
	if err := client.UpdateCampaignBudget(ctx, campaign.PlatformCampaignID, wire, lifetime); err != nil {
		werr := fmt.Errorf("write linkedin campaign budget for campaign %s: %w", campaign.PlatformCampaignID, err)
		if linkedin.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBudgetWriteError{err: werr}
		}
		return werr
	}
	return nil
}
