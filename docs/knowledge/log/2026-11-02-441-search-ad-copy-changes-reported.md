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
- Each added line is a note on the `adPlan` (`copyNotes`), reported as an "Ad copy: …"
  step beside the ad ("Ad N copy: …" when a group has several ads). Drops are bounded: one
  note for the duplicates and one for the overflow, and the walk stops at the cap: rows
  after it are counted, never read past the first, so a long list cannot write a step per
  line or cost work beyond the cap.
- Top-level Headlines/Descriptions are composed only where they can be sent. Off Search
  they never are, and on Search they are not when every requested ad group brings its own
  Ads, so an over-wide line there cannot refuse a create that sends none of it.

`precomputeAdGroupAdInputs` now returns the `adPlan` instead of the two lists, and
`boundedUniqueCopy` is replaced by `callerCopy`. Tests cover the refusal (Search only), the drop and pad
notes, the bounded note count on a 1,000-line list, group copy following the same rule, and
the notes reaching a real create's steps, single-ad and multi-ad;
the refusal and the step reporting are each mutation-checked.
