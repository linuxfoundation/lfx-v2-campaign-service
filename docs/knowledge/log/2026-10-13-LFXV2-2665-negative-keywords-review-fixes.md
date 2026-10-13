# 2026-10-13 — Negative keywords for Reddit and X: review fixes (PR #321)

**Fix** — Five review fixes to `add-negative-keywords`:

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
