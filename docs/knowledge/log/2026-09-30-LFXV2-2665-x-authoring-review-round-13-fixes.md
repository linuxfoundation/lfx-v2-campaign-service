# 2026-09-30 — LFXV2-2665: a boundary rule that only understood English

**Fix** — the thirteenth `/lfx-skills:lfx-local-review` cycle ran against `a182f112` and
returned five items. Two are fixed, two are standing declines, one is a re-raise of an
outstanding branch-history decision that is the maintainer's to make, not a code change.

Both live findings are the same failure that produced rounds 11 and 12: a helper written
with one alphabet, one URL shape and one language in mind, generalised by habit rather than
by evidence. The pattern is now explicit enough to name — **every time this package has
decided what "a URL" or "a word" is, the bug has been in the assumption underneath, not in
the expression.**

## 1. `unicode.IsLetter` made CJK copy invisible to the screen

`urlRunStartIsBounded` — added in round 11 to replace the `\b` that `_` defeated — returned
false whenever ANY Unicode letter preceded a run. The intent was "do not match a scheme
buried inside a longer word". In CJK there are no spaces between words, so a link written
directly after Japanese or Chinese text is not buried in anything; it is a link, and X
linkifies and publishes it.

Measured, before the fix: `登録events.example/r?access_token=PLAINTEXT` was dropped from the
candidate set and published unscreened.

The contradiction was already sitting in the same file. Both scanners' stop sets list
`。`, `、`, `！`, `？`, `，`, `：`, `；` **precisely because a CJK sentence ends without a
space** — and then the boundary helper turned around and used the absence of that space as
proof the link was not a link. The rule is now ASCII-only: a run is bounded unless an ASCII
alphanumeric precedes it.

The cost of the narrowing is an accented Latin letter hard against a scheme — `caféhttps://…`
now reads as a link start. That is not a shape real copy has, and it errs toward a refusal
rather than a publication.

## 2. Scheme-less `user:password@host` was screened by nothing

The scheme-less scanner requires a `?` or `#`, because a query is the only thing it reads.
Userinfo is not in the query. So `bob:pw@events.example` — no query at all — passed both
the publication screen and the snapshot redactor, while the scheme-ful
`https://bob:pw@events.example` beside it was refused outright by one and blanked by the
other. The password is published verbatim either way; whether X renders the run as a link
does not change that the bytes go out in the tweet.

A third pattern covers it on both sides. **The colon is the entire discriminator and the
reason this fix is narrow.** Without it, the pattern matches `bob@events.example` — an
ordinary email address, a shape real tweet copy has constantly — and a screen that refuses
those is worse than the hole it closes. `user:password@host` is the RFC 3986 userinfo
production with a password in it, and nothing else in prose looks like that. A userinfo
with no colon carries no password and is not matched.

On the snapshot side the run is replaced by NOTHING rather than reduced to its host, and
the pass runs LAST. Last because it needs no query: earlier, it would take a query-bearing
run down to its host before the pass responsible for queries ever saw it.

## Declined, with reasons

- **Base emoji clustering on twitter-text's generated sequence data.** Verified real:
  `emojiClusterLen` absorbs a variation selector after any supplementary-plane rune, so a
  non-emoji supplementary ideograph followed by `U+FE0F` is charged 2 where twitter-text
  charges 4. Declined for the same reason as the TLD-registry decline in round 11, and the
  cost direction settles it. This UNDERcounts, so the failure is a create that succeeds and
  an authoring step that X then rejects — the documented non-fatal degraded path, with the
  campaign and line item still returned. Overcounting would refuse a working brief before
  anything is created. Importing a generated emoji table to trade the cheap failure for the
  expensive one is not a trade worth making.
- **Mask the host in the redactors.** Eighth time, unchanged. The knowledge-base rule is a
  two-part test — reproduce a component only when it is BOTH structurally incapable of
  holding a secret AND load-bearing for the diagnosis. "Which site did this link point at"
  is the whole of what a redacted URL still tells the human reading a snapshot. Masking it
  to `https://xxxxx` tells them nothing and closes nothing the path-and-query reduction has
  not already closed.

## Not a code finding

**`95a4471` renders promotable user IDs into errors.** Correct, and already known: that
commit is on the remote, the message form was corrected in a later commit, and removing it
from the range requires rewriting published branch history. That is a maintainer decision
about a force-push, not something a fix commit can do, and it stays open.

## Coverage

Each fix was verified by reverting it alone from a scratchpad copy:

- Restoring `unicode.IsLetter` to the boundary fails
  `TestRejectCredentialQueryParamsInText_CJKAdjacentLink` on the two scheme-less rows,
  printing the surviving credential.
- Dropping `schemelessUserinfoRunRe` from `findSchemelessScreenRuns` fails
  `TestRejectCredentialQueryParamsInText_SchemelessUserinfo` on all three rows; dropping
  the third snapshot pass fails `TestSanitizeSnapshotText_SchemelessUserinfo` on all three
  redaction rows.

One row's evidence is partial, and it is the same shape as round 11's: the CJK test's
SCHEME-FUL row (`詳細はhttps://…`) still passes with the wide boundary restored. The
scheme-ful scanner discards it, and the scheme-less scanner then matches the tail from
`events.example` — whose preceding byte is `/`, a delimiter under either rule — so the
credential is caught by a route that was not written for it. The fix is load-bearing for
the bare scheme-less row, which the test pins, and redundant for the scheme-ful one.
Recorded rather than claimed.

Both negative sides are pinned too: an email address, a time of day (`9:30`), a ratio
(`3:4@events`) and an ASCII word hard against a scheme are all left alone.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (why the boundary
is ASCII, and the third scanner) and `docs/knowledge/code/internal-dispatch.md` (the third
pass, why it runs last, and the colon requirement).

Refs: LFXV2-2665
