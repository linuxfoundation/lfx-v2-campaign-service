# 2026-08-18 — LFXV2-3314 zero_delivery is paid-ads only and suppresses the pacing item

**Fix** — two defects in the action-item rules, both raised by copilot on #137.

**Two HIGH items with opposite remedies.** A campaign that never started is trivially at 0% of
plan, so `zero_delivery` and `underspending` fired together: one says no budget change will fix
this, the other says to adjust the budget. The operator had to pick. `zero_delivery` now
suppresses the pacing item — the comment above the rule already claimed the two were distinct,
and the code did not honour it.

**The email channel cannot use spend as a delivery signal.** HubSpot charges nothing per send and
its adapter always reports `CostMicros: 0`, while mapping opens onto `Impressions`. A delivered
email nobody opened is therefore numerically identical to a paid campaign that never served, and
`zero_delivery` told the operator to check targeting and approval for an email delivered exactly
as intended. `Input.BillsPerDelivery` gates the rule, wired from `Provider.Kind() ==
ChannelPaidAds` — keyed on the CLASSIFICATION rather than the provider so a second email provider
inherits it, and because an unrecognised provider returns `""` and so fails closed.

**Both fixes needed an ENDPOINT test, not just a rules-package one.** In both cases the
rules-package tests passed while the caller could be reverted with nothing failing: hardcoding
`BillsPerDelivery: true` at the call site broke no test until `TestGetBriefMetrics_EmailWithNoOpens
IsNotZeroDelivery` existed. A guard fixed in the package it lives in is only half-pinned — the
layer that decides what to pass needs its own test.

**Design description corrected too.** It claimed `pacing` was absent on an `ok` row with no
budget; the service always sends the object with `label: "unknown"`, and the endpoint tests
require that. Omitting it would make "no pacing" indistinguishable from an older server that did
not send one.
