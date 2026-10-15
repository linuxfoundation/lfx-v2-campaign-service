# 2026-10-13 — Negative keywords for Reddit and X: review fixes (PR #321)

**Fix** — Eight review fixes to `add-negative-keywords`:

- **Reddit's 2,000 limit counts null elements.** The write keeps every element it read, null ones
  included, so the limit is now checked against the number of array elements, both in the
  dispatcher and in the client. Before this, 1,999 keywords plus one null passed the check and
  sent a 2,001-element array.
- **X matches an existing negative regardless of case.** The create path already accepts X's
  case-normalised echo, so the `ALREADY_PRESENT` check now compares the keyword ignoring case. A
  repeat request no longer creates a duplicate criterion.
- **The published request example is valid.** It now uses `Broad`, the only match type Reddit
  (the one platform that takes `revision`) accepts.
- **Docs say the flag gates both writes.** `REDDIT_KEYWORD_TARGETING_WRITES_ENABLED` turns on
  both Reddit targeting writes, the keyword removal and the negative-keyword add. The README,
  chart values, constant and deployment concept now say so.
- **A Reddit batch keeps its `ALREADY_PRESENT` results when the write fails.** A batch can mix
  keywords already excluded with new ones. Before this, a failed PATCH or a failed confirmation
  (re-read error, a different excluded list, another targeting dimension changed) made the whole
  call fail, which threw away the `ALREADY_PRESENT` results that were known before the write. Now
  those items keep their result and each new keyword takes the write's outcome: `FAILED`/`REJECTED`
  when Reddit refused it (new `reddit.IsPlatformRefusal`), `FAILED`/`NOT_SENT` when it never left,
  and `UNCONFIRMED` when the outcome is unclear. This matches how X reports per item. The whole
  call fails only when every item depended on that write.
- **One case-folding key everywhere.** All case-insensitive keyword comparisons now use
  `textfold.Key` (new `internal/platform/textfold`). It maps each rune to the smallest rune of its
  `unicode.SimpleFold` orbit, so it agrees exactly with `strings.EqualFold`. It covers request
  duplicates (the shared validator and Microsoft's), X's already-present lookup and X's echo check.
  `strings.ToLower` keeps σ and ς apart, so before this "ΟΣ" and "ος" counted as the same keyword
  in the echo check but as different keywords in the lookup and duplicate check.
- **`NOT_SENT` counts as a definite outcome.** An X item cut off by the deadline, or by a pacer
  wait that was interrupted, is now kept in the per-item results. Before this, a batch whose only
  sent keyword was `UNCONFIRMED` returned a batch-wide unconfirmed error and lost the `NOT_SENT`
  items. Reddit now follows the same rule: a definite `REJECTED` or `NOT_SENT` write is answered
  per item. Both platforms return a batch-wide error only when every item is `UNCONFIRMED`. The
  dispatch and service code docs now list Reddit and X as implementing `NegativeKeywordAdder`.
- **Microsoft and X refuse an empty `revision`.** Converting the pointer collapsed
  `{"revision": ""}` into the same empty string as an absent field, so Microsoft and X accepted it
  even though the contract says they refuse any revision. The handler now checks whether the field
  is present, not its value, and returns 400 for a non-Reddit campaign before any dispatcher runs.
  On Reddit the value is passed through, and the adapter already refuses an empty or blank one with
  400. The design text now states both rules.
