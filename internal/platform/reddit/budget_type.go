// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Create-time budget pacing (#549 gap 9): CampaignInput.BudgetType chooses the campaign's
// goal_type. The create always sent LIFETIME_SPEND; DAILY_SPEND makes BudgetUSD a per-day cap
// instead. The budget lever, the settings readback and the account monitor already read both
// goal types, so only the create changes. No live create has sent DAILY_SPEND on a CBO campaign
// with an end_time, which is why the dispatcher keeps it behind REDDIT_DAILY_BUDGET_ENABLED.

const (
	// BudgetTypeLifetime and BudgetTypeDaily are the accepted CampaignInput.BudgetType values
	// (redditConfig.budgetType).
	BudgetTypeLifetime = "lifetime"
	BudgetTypeDaily    = "daily"

	// FieldBudgetType is the redditConfig key a budget-type refusal names.
	FieldBudgetType = "budgetType"
)

// ResolveBudgetGoalType returns the campaign goal_type for a budget type: "" or "lifetime" is
// LIFETIME_SPEND (the create's long-standing behaviour) and "daily" is DAILY_SPEND, matched
// case-insensitively after a trim. Anything else is refused with a service-authored sentence; the
// caller's value appears only through domain.CallerValue.
func ResolveBudgetGoalType(budgetType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(budgetType)) {
	case "", BudgetTypeLifetime:
		return GoalTypeLifetimeSpend, nil
	case BudgetTypeDaily:
		return GoalTypeDailySpend, nil
	default:
		return "", fmt.Errorf("budgetType %s is not one of %s, %s", domain.CallerValue(budgetType), BudgetTypeLifetime, BudgetTypeDaily)
	}
}
