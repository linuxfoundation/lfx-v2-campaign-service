# 2026-11-13 — #584 LinkedIn lifetime budget with a daily cap

**Update** — LF often sets a LinkedIn campaign's lifetime budget and a daily cap together (OSS
Japan 2026: $1,000 lifetime, $20 a day). The create path sent `dailyBudget` OR `totalBudget`,
never both.

- `linkedInConfig.dailyBudgetUsd` (optional) adds the cap. With `lifetimeBudget: true`,
  `budgetUsd` is sent as `totalBudget` and `dailyBudgetUsd` as `dailyBudget` on one campaign.
- That campaign is POSTed with `runSchedule.start` and no end. This is LinkedIn's documented
  "Daily Budget and a Total Budget with a Continuous Schedule" option (Create and Manage
  Campaigns, li-lms-2026-02). Campaign Manager also allows only a continuous schedule when both
  budgets are set (help answer a422101). The campaign group keeps the end date.
- Without the cap nothing changes: one budget field, and `runSchedule.end` on the campaign.
- Synchronous 400s in `PreflightCreate`, repeated in `Dispatch` with nothing created:
  - the cap without `lifetimeBudget: true`;
  - a non-number or negative cap;
  - a cap above the total;
  - a cap under $10, or a total under $100.
  Amounts are compared after rounding to cents (`linkedin.ValidateBudgetPair`,
  `CheckDailyCapWithinTotal`).
- Budget write: a campaign carrying both fields used to be refused outright (409). It is now
  paced `lifetime`. A `lifetime` write sets `totalBudget` only and leaves the cap. A total below
  the cap is 400. A `daily` request is 409.
- Account monitor: such a row is paced against the daily cap when it has no end, and otherwise
  against the smaller of the two expectations.
- Tests:
  - `budget_create_test.go`: wire bodies for the pair, the unchanged single-budget creates, and
    refusals with no POST.
  - `linkedin_budget_pair_test.go`: the preflight table and dispatch-to-wire wiring.
  - `TestLinkedIn_WriteBudget_LifetimeWithDailyCap`, plus two guard cases.
  - `TestLinkedinPacingPct_LifetimeWithDailyCap`.
