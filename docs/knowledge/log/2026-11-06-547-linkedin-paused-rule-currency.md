# 2026-11-06 — #547 LinkedIn's paused-campaign rule is sized in the account's currency

**Fix** — follow-up to #454 / PR #532. LinkedIn's "Campaign is PAUSED with real budget" rule
still compared account-currency budgets with USD-sized literals (`TotalBudget > 10 ||
BudgetDay > 1`), so a JPY campaign at ¥2/day counted as real budget and a KWD account's 1/day
(about $3) did not.

- `EvaluateLinkedInMonitor` takes the read's currency, as Google, Microsoft and X do. No extra
  request: `LinkedInDispatcher.ReadAccountMonitor` already returned it (the campaign budgets'
  `currencyCode`, `linkedin.Client.ListAccountCampaignsWithCurrency`); the service did not pass it on.
- The rule calls `rules.isRealBudget`: a lifetime budget above `pausedRealTotalBudgetMinUSD`
  (10) or a daily budget above `placeholderBudgetMaxUSD` (1, the placeholder line read from the
  other side), each sized through `monitorUnitsPerUSD` and compared at the currency's minor unit
  via the new shared `aboveAccountThreshold`, which `isHighAvgCpc` now uses too.
- An absent or unlisted currency fires the rule on no amount and adds the account-wide LOW
  skipped item (`moneyRulesSkippedItem`) — but only over the measured, budgeted, PAUSED rows
  (`linkedinPausedBudgeted`): LinkedIn's currency comes from the budgets, so an account whose
  campaigns carry none reports no currency for a reason that skipped nothing.
- Sweep: no other monitor (Google, Microsoft, X, Meta, Reddit, HubSpot) has a remaining
  USD-sized money literal; the rest of their thresholds are percentages and counts.
- Response shape unchanged; the `currency` field's description now names the LinkedIn rule.

Tests: `TestLinkedInPausedRealBudget_SizedInAccountCurrency`,
`TestLinkedInMoneyRuleSkipped_OnlyWhenAPausedBudgetWasThere` and
`TestMonitorLinkedinAdsAccount_RulesSizedInReadCurrency` each fail without the change they pin
(old literals restored, skipped item removed or unfiltered, currency not passed by the service,
amount not rounded to the minor unit).
