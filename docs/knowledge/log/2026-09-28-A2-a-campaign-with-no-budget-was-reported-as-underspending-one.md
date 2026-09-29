# 2026-09-28 — A2: a campaign with no budget was reported as underspending one

**Fix** — behaviour change, operator-facing, on Google, Meta and LinkedIn. Reddit already did this
correctly and is unchanged.

A campaign the platform reports with no budget at all has nothing to pace against. Three of the
four account monitors said otherwise.

Google computed `expectedSpend := BudgetDay * days`, left `pacingPct` at `0` when that was zero,
and handed the `0` to the ladder — where `< 50` means **underspending**. So every budget-less
campaign was reported as failing to spend a budget it does not have, and carried an action item
reading, verbatim:

> Only spending 0% of $0.00/day budget — $25.00 spent vs $0.00 expected

Meta produced the same class of sentence — `Underspending: 0% of budget used ($25.00 of $0.00)` —
and this is the part worth recording. Meta *had* the guard written: `pacingPct, unknown :=
metaPacingPct(...)` followed by `if !unknown`. But all four of `metaPacingPct`'s `return`
statements ended `, false`. The flag was never once set, so the guard was dead code that read, to
anyone scanning the file, exactly like a working one. An earlier test —
`TestMetaPacingPct_UnknownIsAlwaysFalse` — had actually *noticed* and pinned the always-false
return, describing it as "worth confirming against the BFF source". It was pinned as an observation
rather than raised as a defect, and so survived.

LinkedIn's guard was real: `hasBudget` genuinely held these rows off the ladder. What it did not do
is **say** so. The row went out as `MonitorPacingNormal` with `PacingUnknown` left false, which a
consumer reads as "on plan" — a quieter version of the same lie.

All four now route a budget-less campaign through `unknownPacingRow` (`monitor_shared.go`), which
sets `PacingUnknown` and leaves `PacingPct` at its zero value rather than carrying a computed `0`.
That distinction is the whole fix: `0` means "spent nothing against a real budget", which is a
finding; unknown means "there is no budget to have spent against", which is not.
`model.AccountCampaignMetrics.PacingUnknown` already documented this contract — *"absent, not
defaulted"*, and *"a rule engine MUST NOT compute a pacing percentage against a fabricated flight
window"* — so this is the port finally meeting a contract the model had stated all along.
`fetchFailedRow` is now a thin wrapper over `unknownPacingRow`, which is what it always was: one
specific reason pacing is unknown, not a separate state.

**The real signal is not lost, and that is why suppressing the item is safe.** A budget-less enabled
Google campaign is a genuine problem, and `googleActionItems`' `BudgetDay <= 1` rule already raises
it at **HIGH**, saying the accurate thing — the budget is a placeholder — instead of burying it in a
MED pacing complaint whose own numbers are nonsense. The fix removes a false alert and leaves the
true one; `TestBudgetlessGoogleCampaignStillRaisesThePlaceholderBudget` pins exactly that, because
"we silenced the noisy rule" and "we silenced the alert" are one careless edit apart.

Verified rather than reasoned: the new
`TestBudgetlessCampaignIsPacingUnknown_AllPlatforms` was run against the parent commit in a
throwaway worktree and failed on google, meta and linkedin — with the two message strings quoted
above — and passed on reddit. The messages above are that run's output, not a reconstruction.

This was defect 7 of the ported set. Unlike defects 1–5 it has no lfx-self-serve ticket, because it
was found here rather than carried across; the missing ticket is flagged separately.
