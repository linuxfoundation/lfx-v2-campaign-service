# 2026-09-29 — LFXV2-2665: six fourth-round review fixes to X tweet authoring

**Fix** — the fourth pass of the local pre-PR review over `ceca731b..d9ff4b8c`
returned eight code findings across the three reviewers. Six are taken; two are
declined with reasons recorded here, because a decline that leaves no trace gets
re-litigated every round. This is the last review round before the PR: rounds one
through four returned 5, 5, 6 and 6 findings, and round four's are largely in code
this branch TOUCHES rather than introduced — the reviewer widening its radius, not
residue from the previous fixes. That process has no fixed point, and PR review is
the right place for the rest.

**1. Embedded userinfo was published verbatim.** `validateRegistrationURL` rejects
`https://user:password@host/…`, but a link the caller pasted into their own copy
never passes through that validator, and `rejectCredentialQueryParams` read only
query KEYS. A URL with credentials in its userinfo and no query string at all
therefore had nothing for the screen to object to, and went into the tweet.
`docs/api-catalog.md` claimed this was closed while it was open. `u.User != nil`
is now refused outright, before the query is read. Neither the username nor the
password is named — the URL is redacted, which locates the link without the error
becoming the leak it exists to prevent.

**2. `buildTwitterUTMURL` discarded the same parse error, one call site away.** The
round-three fix caught `u.Query()` in the credential screen and missed it here,
where it is worse: the unreadable pairs are not merely invisible, they are
OVERWRITTEN, because the re-encoded query replaces `RawQuery` wholesale. A
registration URL carrying `?ref=partner;session_token=…` would have lost its
routing parameters silently, sent real click traffic to the wrong page, and then
passed the credential screen because only the freshly generated valid query
remained. It now parses `u.RawQuery` explicitly and fails closed.

**3. The bounded key is now a redacted one for the bare case — reversing round
three.** Round three kept the key in the error and bounded it, over a reviewer
asking for it to be dropped. Round four flagged it twice more, and the learnings
reviewer matched
`caller-url-must-be-redacted-before-errors-steps-and-snapshots`, whose rule is that
a component may be reproduced only when it is BOTH structurally incapable of
holding a secret AND load-bearing for the diagnosis. That test is what splits the
case, and the round-three answer applied it to only half. A key written as
`name=value` IS structurally incapable of being the secret — the secret is the
value, which the parser holds separately and which is never rendered — so it is
still named, because the operator cannot find the parameter otherwise. A BARE
component has no `=` behind it and is not a name at all; the whole run landed in
the key position and may be the credential itself, so truncating it bounded the
leak without redacting it. `queryKeysWrittenWithAValue` re-reads the raw query to
tell the two apart, because `url.ParseQuery` cannot: it gives the empty string for
the value of both `?token=` and `?token`. A bare component is now named only as a
category.

**4. An absent `data` field was read as an empty page.** `{"data":[]}` is X saying
there are no more users; `{}` or `{"data":null}` is X not answering. The decode
left `users` nil in both cases and the walk treated them alike, so a malformed
terminal response after a page holding one user allowed that user to be
auto-selected off a list never confirmed complete — publishing under a handle the
caller did not choose. `findByName` already refuses this shape for a weaker reason
(a duplicate create); identity selection has strictly more to lose, so it now
refuses it too.

**5. BMP emoji skin-tone sequences were over-counted.** `emojiClusterLen` required
a supplementary-plane base or an explicit U+FE0F, so `✊🏽` — U+270A followed by
U+1F3FD with no variation selector — was not seen as a sequence and was charged 2
per codepoint, 4 where X charges 2. Same for `1⃣`. That is the OVER-count
direction the concept file says must never happen, because it invents a rejection
of copy X would have accepted and no retry fixes it. A skin-tone modifier or an
enclosing keycap after a BMP base now counts as the request for emoji
presentation, which U+FE0F was previously the only way to make. This cannot err
the other way: a modifier after a base that is not really an emoji is malformed
text, and folding it charges 2 where the per-rune pass charged 3.

**6. The weighted count now runs over NFC.** twitter-text normalises first and X
weighs the normalised form, so a decomposed `é` (U+0065 U+0301) was two runes here
and one character to X — another over-count, refusing near-limit copy X would have
taken. `golang.org/x/text` was already a direct dependency. Normalising is
conservative by construction, since NFC composition never lengthens a string in
runes, and it is used for counting only: the text published is the caller's own
bytes, because silently rewriting an operator's copy is not that function's job.

**Declined: `text` and `as_user_id` as query parameters.** Flagged at confidence
97 as a logging exposure, and it is one — the promotable user id is a linked
pseudonym and tweet text can carry personal data, and both reach proxy and gateway
logs by sitting in the request URL. It is declined here because it is not this
branch's defect: every POST in this client sends parameters in the query, X's Ads
API defines `POST accounts/:id/tweet` that way, and moving one endpoint to a signed
form body changes OAuth 1.0a signature construction on a path that cannot be
exercised against live X from a local checkout. Worth a ticket; not worth a blind
transport change inside this PR.

**Declined: an allowlist or PII classification over query keys.** The userinfo half
of that finding is taken above. The other half would refuse ordinary registration
parameters — the documented contract explicitly promises `code` and `pin` pass —
so it breaks working briefs to close a hypothesis. The existing note stands: an
allowlist cannot be enumerated in advance for parameters nobody controls.

**Declined again: rewriting `95a4471a`.** Unchanged from rounds two and three. The
repo squash-merges every PR, so neither intermediate patch reaches `main`; a
force-push to satisfy a reviewer reading local history leaves the old objects
reachable by SHA anyway, and the range contains a merge commit whose conflict
resolution a squash would discard.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the
userinfo rejection, the named-versus-bare key split, the fail-closed parse in
`buildTwitterUTMURL`, BMP emoji requests, and NFC normalisation of the weighted
count). `docs/api-catalog.md`'s X config table records the userinfo refusal on
both the registration and tweet-text paths, the unparseable-registration-query
refusal, and that a bare query component is named only as a category.

Refs: LFXV2-2665
