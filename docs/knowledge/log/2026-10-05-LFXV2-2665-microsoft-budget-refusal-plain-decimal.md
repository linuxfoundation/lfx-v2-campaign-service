# 2026-10-05 — Microsoft daily-budget refusal names the amount without rounding

**Fix** — PR #249 review: `microsoft.ValidateDailyBudget`'s over-maximum refusal
formatted the requested amount with `%.2f`. That sentence is handed back to the
caller (via `BudgetAmountReason` → `ErrBudgetAmountRejected` → 400), and the
client deliberately never rounds a Microsoft budget because it does not know the
account currency's minor unit — so a rounded figure (`1000000000.005` read as
`1000000000.01`) misstated the very amount being refused. Both the amount and
the maximum now go through `formatBudgetAmount`, the package's plain-decimal
formatter already used by the mutate's amount refusals.
`TestValidateDailyBudget_OverMaxReasonIsPlainDecimal` pins the exact text. See
[internal/platform/microsoft](../code/internal-platform-microsoft.md).
