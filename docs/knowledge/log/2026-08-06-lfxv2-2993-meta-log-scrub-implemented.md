# 2026-08-06 — the claimed metrics log scrub actually lands

**Update** — Both bot reviewers independently caught that the 2026-08-05 fragment claimed a
`safeErrSummary` helper had been added to `internal/service/brief.go`, and that no such helper
existed anywhere in the tree. The claim was wrong: `GetCampaignMetrics`'s two failure-path logs
were still writing `merr` verbatim. Two bots reporting the same thing is strong signal, and here
they were simply right — the doc described work that was never done.

`safeErrSummary` now exists and both call sites use it. (This sentence was itself premature when
first written: only the generic-failure log at `brief.go` used the helper; the
`ErrMetricsWindowUnsupported` branch above it still logged `merr` verbatim, and that branch is
exposed to the same upstream text — an adapter may wrap the sentinel around a platform's own
"unsupported date range" message, which some APIs echo the request value into. Both branches scrub
now, and the claim is true.) It replaces every non-graphic rune with
U+FFFD and caps the result at `errSummaryMaxRunes` (200) plus a trailing ellipsis. The cap counts
runes rather than bytes so a multi-byte upstream body truncates at a boundary instead of splitting
a rune into replacement characters.
