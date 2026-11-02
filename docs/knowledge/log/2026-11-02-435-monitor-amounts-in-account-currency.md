# 2026-11-02 — #435 monitor action items stop calling account-currency amounts dollars

**Fix** — the Google, Microsoft and Reddit account monitors' action items printed amounts with a
hard-coded `$`, but every figure on those paths is the connected ad account's own, unconverted
currency. A EUR or INR account's spend was called dollars, and lfx-self-serve (which prints the
same amounts currency-neutral since linuxfoundation/lfx-self-serve#4807) showed `1,234.50` under
Spend beside `spent $1234.50` in the issue text.

Each amount now renders as `<n> in account currency`, the wording the Meta (`metaMoney`) and X
(`xMoney`) monitors already used: `googleMoney` in `monitor_google.go`, `msMoney` in
`monitor_microsoft.go` (its nominal `msAmountPrefix = "$"` is gone), and the one Reddit
underspending line. Sentences that read "$N/day" now say "a daily budget of N in account
currency". The Goa examples in `design/account_monitor_examples.go` and `design/connection.go`,
documented as exactly what the rule engine emits, were updated and the OpenAPI regenerated.

Not changed: the remedy text "typically $10-50/day for events" (advice, not an amount read off
the account), and the USD-shaped thresholds (`BudgetDay <= 1`, average CPC above 5). LinkedIn,
Meta, X and HubSpot print no `$` amount.

Tests: `TestGoogleActionItems_NoAmountIsCalledDollars` drives every amount-bearing Google arm and
fails on any `$` (mutation-checked); the Reddit and Microsoft assertions were moved to the new text.
