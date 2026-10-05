# 2026-10-05 — Reddit budget writer: review follow-ups

**Fix** — Three Copilot findings on the Reddit budget writer (#251), deferred from that PR so
an approved, behaviour-neutral change was not pushed again:

- `UpdateCampaignBudget`'s refusal of a non-positive micros value is a `budgetAmountError`,
  so its text can reach the caller through `BudgetAmountReason`. It named an internal helper
  (`BudgetMicros`); it now states the constraint ("a positive number of micro-units").
- A 2xx PATCH response whose `data` is not a campaign object is still reported UNCONFIRMED,
  but the JSON decode error is now kept in the error chain for diagnosis. `encoding/json`'s
  errors name a type or one offending character, never the response body, so this echoes no
  upstream content.
- `TestUpdateCampaignBudget_OutcomeClassification`'s failure message printed the negated
  expectation instead of the observed value; it now prints `IsOutcomeUnconfirmed(err)`.
