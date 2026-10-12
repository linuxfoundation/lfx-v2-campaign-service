# 2026-10-06 — Keyword targeting: pre-PR review fixes

**Fix** — four findings from the pre-PR review of the Reddit and X keyword-targeting levers
(LFXV2-2665), before the PR was opened.

- **X last-keyword race.** Two removals of different criteria could each pass the one-shot
  last-keyword guard and together empty a line item. The targeting is now re-listed before each
  DELETE; an item that is now the last positive keyword is not sent (FAILED/`WOULD_EMPTY`), and
  one whose criterion has meanwhile gone is FAILED/`NOT_FOUND`. The residual window between that
  re-list and the DELETE is documented.
- **Reddit round trip pinned.** The write now sends targeting members byte for byte (HTML
  escaping off, body pre-encoded), keeps un-removed keyword elements including `null` ones, and
  tests pin a 20-digit integer, `1.10`, an explicit `null`, a nested object, an unknown member
  with `<` and `&`, absent-stays-absent, and an unchanged other-dimension fingerprint across
  read → write → read. `ReplaceAdGroupKeywords` became `RemoveAdGroupKeywords`.
- **Exact keyword matching.** The Reddit removal no longer trims the requested keyword: it is
  compared and echoed exactly as sent, and an all-whitespace keyword is a 400.
- **Test data race.** The X dispatch tests read handler-written requests under the stub's lock.

See [Keyword Targeting on Reddit and X](../architecture/keyword-targeting-reddit-x.md).
