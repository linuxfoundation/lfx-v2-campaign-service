# 2026-09-29 — LFXV2-2665: six third-round review fixes to X tweet authoring

**Fix** — the third pass of the local pre-PR review over `ceca731b..b2f685af`
returned six findings, all in the surfaces the second round's fixes created. The
learnings reviewer returned none. Five are taken as reported; one is taken in a
narrower form than proposed, and one is solved differently from the proposal
because the repo already had the right precedent.

**1. `u.Query()` discards the parse error, so an unreadable query was cleared.**
`url.URL.Query()` throws away what `ParseQuery` returns and hands back whatever
pairs it managed to decode. A query Go refuses to decode — an unescaped `;`
separator, a bad `%` escape — therefore arrived at the screen as an empty map,
every unparsed pair invisible, and the URL was published. The gate now parses
`u.RawQuery` itself and fails CLOSED on the error: a query it could not read in
full is a query it cannot clear.

**2. The named key is bounded rather than removed.** The reviewer asked for the
key to be dropped from the error entirely. It stays, because it is what makes
the refusal actionable — the operator cannot find the offending parameter
without it, and a parameter NAME is not the secret; the value is, and the value
is never rendered. The kernel of the finding is real though: "key" is whatever
sits left of the first `=`, and a URL ending in a bare `?eyJhbGciOi…` has no `=`
at all, so the whole token lands in the key position. `safeQueryKeyForError`
strips control characters, so the key cannot forge log structure, and truncates
by rune well below any real parameter name.

**3. `JSESSIONID` and `ASP.NET_SessionId` walked past the screen.** `sessionid`
was exact-only, and the two standard spellings of a session cookie in a URL
normalize to `jsessionid` and `aspnetsessionid`, neither of which the exact set
had. `sessionid` moves to the fragment tier. Bare `session` stays exact-only —
`sessionize`, `session_title`, `breakout_session` are real agenda parameters —
and the full `sessionid` collides with none of them, which is exactly the test
the fragment tier is supposed to apply.

**4. The weighted cap is no bound on raw size.** Any URL weighs a fixed 23
however long it really is, so one multi-kilobyte link passed the 280 check and
was then percent-encoded into the tweet-create request URI, rejected as an
oversized URI only after the campaign and line item existed. `maxTweetRawBytes`
(8 KiB) bounds the composed text. It is deliberately far above any real tweet:
the same asymmetry governs — a bound that exists to catch an absurd input must
not be tight enough to refuse copy X would accept — and 280 weighted characters
of four-byte runes is 1120 bytes, so no legitimate brief comes near it.

**5. An unconfirmed list could still be concluded from — solved with the
precedent, not the proposal.** The reviewer asked for `cursorUnknowable` to
become fatal in `resolvePromotableUser`. That is too strong and would break live
accounts: the function read page one alone before it paginated at all, so
failing on a response shape that works today fixes a case that cannot be worse
than the status quo, and the repo's existing log entry records that divergence
as deliberate. It is also the wrong discriminator. `findByName` already solved
the same problem with PAGE FULLNESS — X documents that a page holding fewer than
`count` entities carries a null cursor, so a SHORT page is conclusively the last
one on its own evidence, while a FULL page OWES a cursor. So ending the walk on
`cursorUnknowable` is unchanged, and what is now gated is CONCLUDING from it:
a full page with no usable cursor leaves the list unconfirmed and every
conclusion below the loop — not-found, none-at-all, and the single-candidate
auto-pick — is refused. `count` is requested explicitly, because under X's
default page size whether a body is short or full depends on a number this
client never saw, which is not evidence anything may be concluded from.

**6. An authored `id_str` was taken on trust.** An explicit `TweetID` is held to
`tweetIDRe` and the int64 range before any mutating call; the authored id was
not, and the caller only tests it for emptiness. An arbitrary non-numeric string
in a 2xx body — a proxy's error document, a reshaped field — was therefore
recorded in `AuthoredTweetID`, persisted into `Steps` as the id an operator
would look up, and only rejected at `promoted_tweets`, after the campaign and
line item existed. `extractTweetID` applies the same two checks; an invalid
value returns `""` and takes the existing malformed-success path, reported
UNCONFIRMED. The rejected value is never echoed — it is upstream response text.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the
fail-closed query parse and the bounded key, `sessionid` in the fragment tier,
the raw byte cap beside the weighted one, the page-fullness rule for concluding
from the promotable-user walk, and `extractTweetID`'s id validation).
`docs/api-catalog.md`'s X config table now records the unparseable-query
refusal, the bounded key, the raw request bound, and that a full page with no
usable cursor is refused while a short one resolves normally.

Refs: LFXV2-2665
