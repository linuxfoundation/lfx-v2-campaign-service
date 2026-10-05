# 2026-10-05 — Budget writers: review follow-ups (#248, #251)

**Fix** — Review findings deferred from #251 (Reddit budget writer) and #248 (micros claim), so
approved, behaviour-neutral PRs were not pushed again:

- `UpdateCampaignBudget`'s refusal of a non-positive micros value is a `budgetAmountError`,
  so its text can reach the caller through `BudgetAmountReason`. It named an internal helper
  (`BudgetMicros`); it now states the constraint ("a positive number of micro-units").
- A 2xx PATCH response whose `data` is not a campaign object is still reported UNCONFIRMED,
  but the JSON decode error is now kept in the error chain for diagnosis. That carries no
  upstream value only because `campaignBudgetWire` has string, `json.RawMessage` and `*bool`
  fields alone (a numeric field, a `,string` tag or a `time.Time` would echo input); the type's
  comment now says so, and new outcome-classification rows (array, string and wrong-kind `id`
  data carrying a marker) pin both the UNCONFIRMED class and that the marker never appears in
  the error.
- `TestUpdateCampaignBudget_OutcomeClassification`'s failure message printed the negated
  expectation instead of the observed value; it now prints `IsOutcomeUnconfirmed(err)`.
- `TestUpdateCampaignBudget_RejectsBadRequests` now pins the rounds-to-zero 400's wording
  (zero micros, one micro = 0.000001 of the account currency, under half of one).
- `docs/api-catalog.md` describes 0.000001 as the intentionally stricter HTTP (Goa) contract
  floor rather than the bound "the service actually enforces"; the service's own guard refuses
  only what rounds to zero micros.
- The sub-micro paragraph in the `internal-service` concept is re-wrapped to the file's width.
