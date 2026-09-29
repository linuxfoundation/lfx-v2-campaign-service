# 2026-09-30 — LFXV2-2665: five fixes from the sixth local pre-PR review of X tweet authoring

**Fix** — the sixth `/lfx-skills:lfx-local-review` cycle on the X authoring work
returned six findings against `93d2576e`. Five are fixed here; the reasoning for
the declines is recorded below so it is not re-litigated in a seventh round.

## 1. A stale comment contradicted the contract its own commit implemented

`createNullcastTweet` still said "The other three converge on re-issue" after the
previous commit had flipped the campaign and line-item creates to
`idempotent=false`. Only `promoted_tweets` is retry-safe, because only it
converges on the SERVER (`DUPLICATE_PROMOTABLE_ENTITY`); the find-by-name dedup
for campaigns and line items runs in `CreateCampaign`, ABOVE the retry loop, and
a retry from inside the loop re-POSTs regardless. A comment asserting the
opposite is worse than no comment: the next reader reasons from it.

## 2. Compound credential keys walked through the publication screen

`auth_cookie`, `session_cookie`, `connect.sid` and `PHPSESSID` all passed
`isCredentialQueryKey` and would have been published verbatim in a tweet, while
`docs/api-catalog.md` promised the screen caught credentials "in any
case/separator spelling, compounds included".

The cause is the separator normalizer that makes the rest of the classifier work:
folding `-`, `_` and `.` away turns `auth_cookie` into `authcookie`, a name that
matches no exact entry and contains no listed fragment. The obvious repair —
adding `auth` and `sid` as substrings — is the one that must not be made: `auth`
is inside `author`, `sid` is inside `aside`, `subsidy` and `president`, and all
of those are real registration-page parameters. Refusing them fails a working
brief, which is the exact cost this denylist exists to avoid.

The fix restores the boundary the fold destroys. A COMPONENT tier splits the
ORIGINAL key on `-`, `_` and `.` and tests each part against a small set —
`auth`, `sid`, `pwd`, `passwd`. `author` is one component and does not match;
`auth_cookie` is two and does. `session`, `pass`, `sig` and `key` are
deliberately NOT in that set even though they would be unambiguous components in
another domain: this is an events service, and `session_title`, `session_track`
and `day_pass` are ordinary parameters on a conference registration page.
Separately, `sessid` and `cookie` joined the substring tier — `PHPSESSID`
normalizes to a name containing neither `sessionid` nor any exact entry, and a
parameter carrying a cookie under any name is carrying the session itself.

## 3. The fragment was never screened, and it is where the token actually is

The gate read the query and the userinfo. The OAuth implicit flow returns its
bearer token AFTER the `#` — `https://app.example.org/cb#access_token=…` — so a
URL pasted out of a logged-in browser carries a live token in a URL that may have
no query string at all. `credentialFragmentError` now runs the same key screen
over a fragment written in `key=value` form. A plain `#register` or
`#agenda-day-2` has no key, passes, and must: a section anchor is how a brief
links into a registration page.

This was raised in three consecutive reviews before being fixed.

## 4. The URL-run boundary lost CJK sentence punctuation and angle brackets

`tweetURLRe` was `\S+`, so `詳細 https://lfx.dev。` swallowed the ideographic full
stop into the link's fixed t.co weight of 23 instead of charging it its own 2.
That is the UNDERCOUNTING direction, and undercounting at the 280 boundary means
the campaign and the line item are created and X refuses the tweet afterwards —
the avoidable degrade the pre-create guard exists to prevent. LF runs KubeCon
China and Open Source Summit Japan; a CJK sentence puts no space before its stop,
so this is a realistic brief, not a constructed one.

Trimming the tail cannot fix it: in `…lfx.dev、そして` the comma is in the MIDDLE
of the whitespace-delimited run, and a trailing trim never reaches it — the whole
Japanese tail vanished into the 23. So `。、！？，：；` and `<`/`>` are excluded
from the run itself rather than trimmed off it. None is legal unescaped in a URL,
so the run loses nothing. `trimTweetURLPunct` also now walks by RUNE rather than
by byte; a byte-wise loop can only ever trim ASCII, and comparing one byte of a
multi-byte character against an ASCII table compares against a fragment of a
character.

## 5. The retained transport cause was an exported field

`transportError.Err` and `preSendError.Err` held the `*url.Error` out of
`http.Client.Do`, which carries the full request URL. `Error()` was already
clean — `safeTransportCause` peels every `*url.Error` layer — but a clean
`Error()` closes only the channel that renders the struct as a string.
Reflection- and JSON-based logging walks exported fields and never calls
`Error()`, so the URL was one structured log call away. All three types
(`apiError` included, for consistency) now hold the cause in an unexported `err`
and keep `Unwrap()`, which is the shape
`platform-error-must-not-carry-untrusted-or-credential-text` prescribes
verbatim: "kept for `Unwrap()` only and never rendered or exported — that is the
correct shape". The rename is compiler-verified across the package.

## Declined, with reasons

- **Move `text`/`as_user_id` out of the query string.** Unchanged from round 4:
  the transport change is larger than the exposure, which is already closed at
  every rendering site.
- **An allowlist of "safe causes" inside `safeTransportCause`.** After every
  `*url.Error` layer is peeled, what remains is a `net.OpError`, a syscall errno
  or `io.EOF` — none of which carries a URL. An allowlist would discard real
  diagnostic detail to guard against a cause that does not occur, and would fail
  silently the day a new one did.
- **Drop the HOST from `sanitizeSnapshotURL` / `redactURLForError`.** The
  knowledge-base rule is a two-part test: reproduce a component only when it is
  BOTH structurally incapable of holding a secret AND load-bearing for the
  diagnosis. A host is incapable, and it is the only thing that tells the
  operator which link in their copy is the offending one. `redactAIProxyURL` is
  not the precedent it looks like — there the internal proxy hostname WAS the
  secret.
- **A PII allowlist over query keys, values and paths.** An allowlist over an LF
  event page's routing and attribution parameters refuses working briefs to
  protect against nothing; the denylist shape was chosen for that reason and the
  reasoning has not changed.

## Coverage

Every fix has a test verified to FAIL against the pre-fix code, not merely to
pass against the new code: `TestIsCredentialQueryKey_CompoundComponents` (six
keys published before), `TestRejectCredentialQueryParams_ScreensTheFragment`
(the implicit-flow token cleared before, plus benign anchors that must still
pass), and three new rows in `TestWeightedTweetLen_URLRunBoundaries` (ideographic
stop, ideographic comma, angle brackets). The `err` rename is compiler-verified
and covered by the existing `TestTransportError_DoesNotLeakURL` /
`TestPreSendError_DoesNotLeakURL`.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md`.
`docs/api-catalog.md` updated for the classifier, the fragment screen and the
run-boundary rule.

Refs: LFXV2-2665
