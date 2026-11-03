# 2026-11-03 — #448 follow-up: the placeholder-budget advice names no dollar figure

**Fix** — The Google and Microsoft monitors' placeholder-budget item still advised "Set a real
daily budget (typically $10-50/day for events)", the last `$` those monitors printed after #435
moved every amount to "in account currency". Both now give X's currency-neutral advice: "Set a
realistic daily budget for the campaign's goal and audience before expecting results"
(`internal/service/rules/monitor_google.go`, `monitor_microsoft.go`).

Tests: `TestGoogleActionItems_NoAmountIsCalledDollars` now checks each item's action as well as
its issue, and `TestMicrosoftActionItems_NoDollarSign` does the same for Microsoft. Both fail
with the old advice restored.

Not changed: the placeholder trigger (`BudgetDay <= 1`) and the high-CPC threshold are sized
for USD, so they misfire on other currencies. They change behaviour, not wording, and are
tracked separately.
