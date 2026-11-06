# 2026-11-06 — #454 Monitor money thresholds are sized in the account's currency

**Fix** — the account monitors' placeholder-budget rule (daily budget ≤ 1) and high-CPC rule
(average CPC > 5) compared account-currency amounts with USD-sized numbers, so a JPY account
tripped the high-CPC rule on a ¥50 click and a KWD account's 1/day budget (about $3) was flagged
HIGH as a placeholder.

- Option 1 of the issue (a per-currency table): `rules.monitorUnitsPerUSD` holds rounded
  units-per-dollar magnitudes, and `isPlaceholderBudget` / `isHighAvgCpc` in `monitor_shared.go`
  scale the USD values (`placeholderBudgetMaxUSD`, `highAvgCpcUSD`) through it. Nothing is
  converted or displayed with it. Option 2 (an FX rate source) would add a dependency for two
  heuristics; option 3 (relative thresholds) would change what both rules mean.
- `EvaluateGoogleMonitor`, `EvaluateMicrosoftMonitor` and `EvaluateTwitterMonitor` take the
  read's currency. X's placeholder rule is the same rule (`xPlaceholderBudgetMax` was Google's
  value), so it moved too; Microsoft's `msPlaceholderBudgetMax` / `msHighAvgCpc` are gone.
- A currency the table does not list (high-inflation ones are left out on purpose) or an absent
  one fires neither rule on an amount; Google's and Microsoft's zero daily budget is still a
  placeholder.
- Google Ads now reads its currency: the monitor's GAQL query selects `customer.currency_code`
  (an attributed resource of `campaign`, so no extra request), and `GoogleAdsDispatcher` implements
  `ReadAccountMonitor`. The monitor's `currency` field, previously never set on Google Ads, is now
  published when every row carries the same well-formed code. The field's shape is unchanged.
- Not live-verified: no Google Ads request was sent. The GAQL field is documented in the Google
  Ads API v23 `campaign` resource's attributed `customer` fields.

Tests: `TestMonitorMoneyRules_SizedInAccountCurrency`, `TestTwitterPlaceholderBudget_SizedInAccountCurrency`,
`TestMonitorAccount_MoneyRulesUseTheReadsCurrency`, `TestListAccountCampaigns_ReadsCustomerCurrency`,
`TestGoogleAds_ReadAccountMonitor_CarriesCurrency` and `TestGoogleAdsAccountCurrency` each fail
without the change they pin.
