# 2026-10-15 — The X name lookup refuses a page with a repeated key

**Fix** — a review thread on #358 (LFXV2-2665).

- `findByNameChecked` (the campaign and line-item reuse lookups) decoded each page without
  `identityjson`. A row carrying `"deleted":false,"deleted":true`, or a case-folded `"Deleted"`,
  decoded last-wins as deleted; the new deleted-match skip then dropped it, and with no other
  match the lookup answered not-found, licensing a duplicate create.
- Each page's raw body is now checked before decoding; a refused page is a lookup error, so the
  caller aborts before any create.
- `TestFindByName_DuplicateKeyPageIsAnError` covers both duplicate-key forms through
  `findByName` and `findLineItemByName`; removing the check fails both.
- Updated [internal/platform/twitter](../code/internal-platform-twitter.md).
