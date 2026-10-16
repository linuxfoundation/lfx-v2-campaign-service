# 2026-10-15 — The X promoted-tweet reuse read checks its bytes and counts associations once

**Fix** — three review threads on #358 (LFXV2-2665).

- `listPromotedTweets` decoded each page without `identityjson`, so a body with a duplicated
  `data` key was read last-wins: a trailing empty array read as "no associations" and would
  license authoring and promoting a second tweet. Each page's raw body is now checked first,
  and a refused page fails the read closed like every other unreadable page.
- Associations were counted by row, so X repeating one association on two cursor pages made
  both retry paths refuse a reuse as ambiguous. They are now keyed by id: a repeat is skipped,
  and one id naming two different tweets is refused.
- `TestListPromotedTweets_FailsClosed` gains the duplicated-key and one-id-two-tweets cases;
  `TestCreateCampaign_RepeatedAssociationAcrossPagesIsReused` covers the overlapping-page walk
  on both the authored-text and explicit-TweetID retry paths. Removing either guard fails them.
- Updated [internal/platform/twitter](../code/internal-platform-twitter.md).
