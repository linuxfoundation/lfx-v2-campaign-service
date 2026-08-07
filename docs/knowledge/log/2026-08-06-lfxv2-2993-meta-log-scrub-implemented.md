# 2026-08-06 — the claimed metrics log scrub actually lands

**Update** — Both bot reviewers independently caught that the 2026-08-05 fragment claimed a
`safeErrSummary` helper had been added to `internal/service/brief.go`, and that no such helper
existed anywhere in the tree. The claim was wrong: `GetCampaignMetrics`'s two failure-path logs
were still writing `merr` verbatim. Two bots reporting the same thing is strong signal, and here
they were simply right — the doc described work that was never done.

`safeErrSummary` now exists and both call sites use it. It replaces every non-graphic rune with
U+FFFD and caps the result at `errSummaryMaxRunes` (200) plus a trailing ellipsis. The cap counts
runes rather than bytes so a multi-byte upstream body truncates at a boundary instead of splitting
a rune into replacement characters.
