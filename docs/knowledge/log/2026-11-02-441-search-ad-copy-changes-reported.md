# 2026-11-02 — #441 Search ad copy is refused when too wide, and every change is reported

**Fix** — `composeAdCopy` (`internal/platform/googleads/ad_copy.go`) used to rewrite an
operator's Search copy without saying so: it cut over-long lines mid-word, dropped
duplicates and lines past the count cap, and padded a short set with generic lines such as
"Save Your Spot Now". The step line reported only the final counts. lfx-self-serve caps
each line at 30 or 90 characters but does not require 3 headlines or 2 descriptions, so an
operator who wrote one headline got an ad with two lines they never wrote or saw.

Now:
- A caller line over Google's weight (CJK and full-width characters count double) is
  REFUSED in the pure preflight, before the budget mutate, as the Display, Demand Gen and
  Performance Max creatives refuse theirs. Google would refuse it after the campaign existed.
- Duplicates and lines past the cap (15 headlines, 4 descriptions) are still dropped, and a
  short set is still padded: refusing either would fail Implement-tab creates that work
  today, since its Add buttons have no cap and it does not require the minimums.
- Each drop and each added line is a note on the `adPlan` (`copyNotes`), reported as an
  "Ad copy: …" step beside the ad ("Ad N copy: …" when a group has several ads).

`precomputeAdGroupAdInputs` now returns the `adPlan` instead of the two lists, and
`boundedUniqueCopy` is replaced by `callerCopy`. Tests cover the refusal, the drop and pad
notes, group copy following the same rule, and the notes reaching a real create's steps;
the refusal and the step reporting are each mutation-checked.
