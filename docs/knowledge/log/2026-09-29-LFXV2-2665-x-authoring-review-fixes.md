# 2026-09-29 — LFXV2-2665: five review fixes to X tweet authoring

**Fix** — the local pre-PR review of the seven H1 commits against `origin/main` returned
seven findings across its three reviewers. Two pairs described the same defect from
different angles and two described the same test, leaving five fixes, all landed in one
commit. None of them is about the merge; the authoring feature had simply never been
through this harness.

**1. A 429 on tweet authoring was retried, and a retry publishes a second tweet.**
Retry eligibility is now an explicit `idempotent bool` threaded through `request` ->
`createRequest` -> `doRequest` -> `doRequestAbs`, and a non-idempotent call takes the
retry-exhausted exit on its FIRST 429. The HTTP method cannot decide this — X answers a
429 at or after committing the write it throttled — so what decides it is whether the
endpoint converges on a repeat. Three of this client's four creates do: campaigns and
line items are found-or-created by name, a repeated `promoted_tweets` POST returns
`DUPLICATE_PROMOTABLE_ENTITY`. A tweet has no name and no idempotency key, so
`createNullcastTweet` passes `false`, alone in the package, and the throttle surfaces as
an `*apiError` that `createOutcomeAmbiguous` renders UNCONFIRMED — a human check before a
second publish. This is the same convention the googleads client uses for its POST
`:search` / POST `:mutate` split.

**2. `resolvePromotableUser` named the account's promotable user ids in its errors, and
read only page one.** Those strings become the campaign's persisted
`PromotedTweetWarning` and a `Steps` entry, both rendered, so the ids reached every reader
of the campaign; they now carry the COUNT only, which is the part that makes the message
actionable. The list is also walked with `cursorVerdict` under `maxListPages` with cursor
dedup: reading page one alone reported a pinned user on a later page as not promotable at
all, and fired the single-candidate shortcut on an account whose later pages held more —
auto-picking an author the caller never chose, against this client's own "never silently
pick" rule. It departs from `ListAdAccounts` on one point deliberately: `cursorUnknowable`
ends the walk rather than failing it, because this function paginated not at all before,
so a hard error on a response shape that works today would break live accounts to fix a
case that cannot be worse than the status quo.

**3. `weightedTweetLen` counted runes where X counts WEIGHT.** Outside twitter-text's
weight-1 ranges (`[0,4351]`, `[8192,8205]`, `[8208,8223]`, `[8242,8247]`) every rune costs
2, so 280 CJK characters were accepted here and refused by X. The two directions of error
are not symmetric and that decided the emoji handling: an under-count wastes a round trip,
an OVER-count invents a rejection of copy X would have accepted, which no retry fixes and
no error explains. `emojiClusterLen` therefore collapses a presentation sequence into one
2-weight cluster — skin tone, ZWJ family, keycap, country flag each cost 2 in total, where
a per-rune pass charged a family sequence 14. A BMP codepoint starts a cluster only behind
U+FE0F, so a bare `©` stays weight 1. This is also what makes the pre-create rejection
`docs/api-catalog.md` promises actually true.

A related exposure found in the same area: `composeTweetText` PUBLISHES the registration
URL's pre-existing query verbatim, and a link pasted from a logged-in browser can carry a
session token. `rejectCredentialQueryParams` now refuses that, pre-create, before the
campaign and line item exist. It is a DENYLIST and not an allowlist on purpose — LF event
pages carry routing parameters nobody can enumerate, so an allowlist would refuse working
briefs to protect against nothing. `code` and `pin` are weighed and excluded: a discount
code is not a credential. The error names the KEY, never the value.

**4. `pace(ctx)` ran before `resolvePromotableUser`, not before the write.** `pace`
RESERVES the next write slot rather than holding one open, so the unpaced
`promotable_users` GET sat inside the reservation and a concurrent writer sharing the
client — the dispatch layer shares one per connection, deliberately — could reserve and
issue in that window, landing two writes together. Reads cost nothing to move, so the
resolve runs first and the reservation is taken immediately before
`createNullcastTweet`.

**5. `TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID` hit its window
by timing.** It bet a 50ms sleep against a 500ms write delay; the bet was a good one and
still a bet, and every way of losing it landed on one of the two silent-pass modes its own
comment documents. A new `onPaceWait` hook — `onAdmit`'s complement, fired under `writeMu`
just before `sleepCtx`, nil in production — makes that window an ordinary synchronous
callback, and the test cancels from it when `rec.Calls() == 1`. No sleeps remain.

**Also, and separately from the client:** `config_snapshot` is persisted UNENCRYPTED, and
`campaignFromTwitter` marshalled `tweetText` into it verbatim. `sanitizeSnapshotText`
(`internal/dispatch/creds.go`) rewrites every http/https run in free text through the
existing `sanitizeSnapshotURL`, as `reddit.go` and `meta.go` already do for their URL
fields, and is applied to a COPY so the text sent to X is untouched. This is not redundant
with fix 3's denylist: that one refuses credential-shaped parameters because the text is
about to be published, cannot name every credential parameter a registration page might
use, and does not apply to rows written before it existed. The snapshot does not have to
guess, so it keeps nothing.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (retry
eligibility, promotable-user counts and pagination, weighted length and the emoji
collapse, the credential-query denylist, the pace ordering and `onPaceWait`) and
`docs/knowledge/code/internal-dispatch.md` (a new section on why `config_snapshot` strips
URLs). `docs/api-catalog.md`'s X config table lost the now-false claim that the
promotable-user refusal "names the candidates found" and gained the weighted-cap and
credential-parameter behaviour.

Refs: LFXV2-2665
