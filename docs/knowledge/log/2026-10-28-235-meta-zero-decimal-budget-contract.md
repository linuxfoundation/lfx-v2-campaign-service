# 2026-10-28 — Meta zero-decimal budgets: the recorded budget is the one Meta was sent

**Verification** — closes #235. `campaigns.budget_amount` records the amount a dispatch or a
budget change asked for. On a Meta account in a zero-decimal currency (JPY, KRW) that amount
used to be rounded to a whole unit before it was written. So a requested `1.4` reached Meta as
`1` and was recorded as `1.40`, with a 200 response.

The adapter fix already shipped in #316. `meta.budgetToMinorUnits`, which the create path and
the budget-write path share, refuses an amount finer than one minor unit of the account
currency instead of rounding it. The refusal wraps `ErrBudgetAmountInvalid`, which the
dispatcher maps to `domain.ErrBudgetAmountRejected`, so the caller gets a 400 and nothing is
written. Every amount that is accepted is encoded exactly (amount × offset), so the recorded
amount divided back from the minor units Meta received is the amount that was recorded. That is
option 2 of the issue. Option 1 (the writer returns a confirmed amount) is no longer needed,
because no accepted amount is rounded.

## What this entry adds

- **Tests for the invariant at both write boundaries**, for zero-decimal (JPY, KRW) and
  two-decimal (USD) currencies:
  - `TestCreateCampaign_TheBudgetSentIsTheBudgetRequested` (`internal/platform/meta`) derives
    the offset from the account's own currency, as production does. It asserts the raw
    `daily_budget` value on the ad-set POST, and that a fractional amount is refused before any
    ad set is created.
  - `TestMeta_WriteBudget_SentAmountIsTheRequestedAmount` and a KRW row in
    `TestMeta_WriteBudget_Guards` (`internal/dispatch`) do the same on the budget-change path.
- **The contract says so.** The `budget` attribute of `update-campaign-budget`
  (`design/brief.go`, regenerated with `make apigen`) now states that a Meta amount must be a
  whole number of the account currency's minor unit, and that a finer amount is refused with
  400, never rounded.

Out of scope, as the issue says: sub-unit precision for platforms whose smallest unit is at or
below a cent (Google micros, LinkedIn cents). The settings readback covers those.
