# 2026-09-28 — A2: MED action items sorted behind LOW ones on LinkedIn

**Fix** — behaviour change, operator-facing, LinkedIn only. Refs
`linuxfoundation/lfx-self-serve#3018`.

The account monitors sort their action items HIGH, MED, LOW, then anything unrecognised, matching
the BFF's `generateActionItems` sort key (`{ HIGH: 0, MED: 1, LOW: 2 }` with a `?? 3` fallback).
Each of the four ports carried its own copy of that switch. LinkedIn's spelled the middle case
`"MEDIUM"`, while `model.MonitorPriorityMed` is `"MED"` — so no MED item ever matched, every one
fell through to the unranked bucket at 3, and MED items sorted **behind** the LOW ones.

That is not a cosmetic ordering difference. The action-item list is what an operator reads
top-down to decide what to fix first, so on LinkedIn the least urgent findings were presented
ahead of the middle band on every response.

Four of these deleted, one added: `priorityRank` in `monitor_shared.go`, with `sortByPriority`
moved there alongside it (it had been living in `monitor_google.go` despite all four calling it,
and it no longer needs the rank function passed in). Once LinkedIn's copy is corrected all four
are byte-identical, so keeping four is keeping the next divergence.

**The duplication is the finding, not just its carrier.** Nothing about `linkedinPriorityRank`
looked wrong on its own — it was a well-formed four-arm switch over string constants, and
`"MEDIUM"` reads as the obvious spelling of the middle priority. Telling it from its three correct
siblings meant reading all four files side by side against the enum. A typed constant would have
made it a compile error; one shared function makes it a single place to be right.

`TestPriorityRank_MedSortsAheadOfLow` asserts the fix through the sort rather than against the
rank numbers, since the order is what an operator actually sees, and `TestPriorityRank_Values`
pins the individual ranks so the two can't collide undetected. It includes
`{model.MonitorPriority("MEDIUM"), 3}` deliberately: the misspelling that caused this is not
special, it is just one more unrecognised string, and the fix is that the code no longer contains
it rather than that it is now handled.

The three per-platform `Test*PriorityRank_*` tests are removed with the functions they covered —
including LinkedIn's, which had pinned the bug as intended behaviour.

This was the first of the five ported BFF quirks to be fixed. The old justification for carrying
them was the OLD-vs-NEW differential diff against the still-live BFF; that diff is no longer the
plan of record, which removes the reason, so the "KNOWN BUG, ported verbatim … do not fix this"
comment came out with the code it guarded.
`docs/knowledge/architecture/account-monitor-endpoints.md` now tracks all five with their issue
numbers and current status rather than describing them as deliberately preserved.
