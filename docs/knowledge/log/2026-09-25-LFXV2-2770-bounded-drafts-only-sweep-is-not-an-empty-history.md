# 2026-09-25 — a bounded sweep whose matches were all drafts returned a false absence

**Fix** — moving the state and future-date gates out of the search predicate and into the
loop (2026-09-22) fixed a real 503, and opened a false absence on the other side of the same
split.

`walkEmails` raises `ErrSearchIncomplete` when it stops at its scan bound having accepted
NOTHING. The predicate now deliberately admits drafts, so that the guard keeps measuring
"nothing matched this event" rather than "nothing had gone out" — which is exactly what made
a drafts-only event a 503 before. The cost is that `len(out)` at the guard is no longer the
population the caller keeps: `LastSent` rejects the drafts afterwards.

So a portal holding matching drafts past the bound satisfied the walk, emptied in the loop,
and returned `(empty, nil)`. Reproduced with a 100-row-per-page portal whose every row was a
matching `DRAFT`:

```
WARN hubspot email search stopped at its scan bound ... scanned=2000 pages=20 matched=2000
rows=0 err=<nil>
```

2000 matching rows, the walk never finished reading the portal, and the operator is told the
event has never been emailed — the precise claim `ErrSearchIncomplete` exists to refuse. A
control on the same portal with non-matching names still returned `ErrSearchIncomplete`,
which is what isolates the cause to the predicate/loop split rather than to the bound.

**The fix** — `SearchEmailsMatchingBounded` reports whether the walk stopped at its bound, and
`LastSent` re-raises `ErrSearchIncomplete` when the walk was INCOMPLETE and its own filter
kept nothing. Conditioned on incompleteness, not on an empty candidate list: a walk that read
the portal to the END whose rows were all drafts is a TRUE absence, and raising there would
restore the 503 the split exists to remove.

`SearchEmailsMatching` and `SearchEmails` are unchanged and pass `nil`, so the template
picker's contract is untouched.

## Tests

`TestLastSent_ABoundedSweepWhoseMatchesAreAllDraftsIsNotAnEmptyHistory` and
`TestLastSent_ACompleteSweepWhoseMatchesAreAllDraftsIsAnEmptyHistory` pin both directions
against a paging portal. Disabling the guard fails the first and leaves the second passing,
which is the asymmetry that proves the condition is on incompleteness rather than on
emptiness.

Also added `TestMatchLastSent_AGenericOnlyHitIsFlaggedAsFallback` and
`TestLastSent_GenericOnlyHitsDoNotDeleteTheBrandFallback`: the generic-only demotion — the
headline fix of the 2026-09-22 change — had no test, and reverting `m.Fallback` in the
`Overlap>=2` arm passed all 36 packages.

**Still open** — the two live-portal checks the 2026-09-22 fragment lists as merge gates are
not done, and `isPublished`'s `AUTOMATED_SENT` / `AUTOMATED_AB` states remain unsubstantiated
against a real portal.
