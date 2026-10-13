# 2026-10-13 — Negative keywords for Reddit and X: review fixes (PR #321)

**Fix** — Four review fixes to `add-negative-keywords`:

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
