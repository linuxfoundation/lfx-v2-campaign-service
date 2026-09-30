# 2026-09-25 — LFXV2-2665 the manual workflow always gets its destination template

**Fix** — the X client's manual-workflow block was gated as a single
`else if composedTweetText == ""`, which suppressed the destination-URL template
step along with the "No tweet ID provided" sentence. Every authoring degrade —
refused, failed, or UNCONFIRMED — tells the operator to post a tweet by hand,
and that step is the only place they are handed the UTM'd destination to put in
it. So a caller who supplied `tweetText` and hit an authoring failure got
strictly LESS than a caller who supplied nothing, in the very same manual
workflow, and had to rebuild the `utm_*` set themselves — the divergence
`twitterUTMParams` exists to prevent. The gate is now split: the sentence stays
conditional, the template is unconditional whenever no tweet was promoted.

**Docs** — `docs/api-catalog.md`'s X "Destination URL" paragraph now states the
constraint the `buildTwitterUTMURL` godoc already claimed it stated: on the
`tweetText` path the registration URL, *including its own pre-existing query
parameters*, is published in the tweet and visible in X Ads Manager, so it must
not carry a `?token=…`-style credential. Redaction protects what is persisted;
nothing can un-publish what was sent to X. The godoc's cross-reference was true
of the behaviour but not of the document, which is the worse failure of the two
— the next reader trusts it and stops looking.

**Docs** — corrected a concept-file sentence that described the authoring
cancellation path as "split on `createOutcomeAmbiguous` first". That split
classifies the authoring POST's own outcome; the `pace(ctx)` abort returns a
non-nil partial result unconditionally. The behaviour was safe either way, but
the bundle is what `CLAUDE.md` sends a reader to consult *instead of* the
source, so a mechanism it describes has to exist. Same paragraph: "2xx with no
`data.id`" now says `id_str`, which is what `extractTweetID` actually reads.

**Note** — tests added for all three of the previous entry's fixes, which had
shipped without any:

- `TestWeightedTweetLen_CountsEveryURLAtTcoWeight` — the pre-existing rejection
  test uses text containing no URL, so it passed identically against the
  weight-only-what-was-appended bug. The new cases cover a caller-embedded URL,
  two URLs, and a URL shorter than 23 runes (t.co is a fixed weight, not a cap —
  asserting the direction is what stops a future `min(raw, 23)`).
- `TestBuildTwitterUTMURL_KeepsQueryDropsFragment` — fragment dropped, brief's
  query kept, and the display counterpart still strips it.
- `TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID` — the
  partial-result guarantee. Cancelling inline in the `/tweet` handler kills the
  authoring POST and diverts into the UNCONFIRMED branch, which is non-fatal and
  returns a nil error, so such a test passes while verifying nothing. The
  handler instead spawns a goroutine that cancels after 50ms against a 500ms
  write delay: the POST finishes in under a millisecond and the following
  `pace(ctx)` blocks for the whole delay, so the cancel lands inside that
  window, and a slow machine moves it further in rather than back. Verified by
  mutation — zeroing the closure's `AuthoredTweetID` makes it fail — and stable
  over `-race -count=20`.

Refs: LFXV2-2665
