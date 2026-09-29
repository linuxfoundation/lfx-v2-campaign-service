---
type: "Go Package"
title: "internal/platform/twitter"
description: "X (Twitter) Ads v12 client: OAuth 1.0a signing, ad-account discovery, the campaign -> line_item -> promoted_tweet creation flow, and per-client write pacing toward X's 1-write/sec account limit."
resource: "internal/platform/twitter"
tags:
  - platform-client
  - twitter
  - x-ads
  - oauth1
  - go-package
  - metrics
  - account-discovery
timestamp: "2026-08-05T00:00:00Z"
---

# internal/platform/twitter

Package twitter is the X (Twitter) Ads API v12 platform client. It implements
OAuth 1.0a (HMAC-SHA1) request signing and drives the
campaign -> line_item -> promoted_tweet creation flow. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database.

`CreateCampaign` is only PARTIALLY idempotent: it reuses existing campaigns and
line items by name (paged cursor lookups via `findByName`) before creating new
ones, and a lookup that fails transiently propagates an error so the caller
aborts rather than creating a duplicate. Reuse is NOT a silent no-op: when a
campaign or line item is reused, the client does NOT re-apply this request's
budget/config/flight-dates to it, so the reused resource may be serving under a
DIFFERENT budget/config or an already-ENABLED line item with different dates. This
is signalled two ways for reconciliation — a warning step in the result, and the
structured `CampaignResult.Reused` flag (set on both the success and partial-result
paths). Consumers that need to know whether the returned campaign matches the
request MUST inspect `Reused` (the dispatch adapter maps a reused result to the
`created_degraded` status, and an authoritative reconcile is the orchestrator's
job, LFXV2-2665). The promoted-tweet association, however,
is always re-POSTed on a repeat call. A recognizable duplicate response
(`DUPLICATE_PROMOTABLE_ENTITY`) is NOT treated as idempotent success: X returns
that code even when the tweet is already promoted by a DIFFERENT line item, so it
is surfaced as a warning (on `PromotedTweetWarning` and in the step log) to be
verified manually rather than assumed to attach to this line item. A
lost/malformed first response likewise produces a warning. True cross-call
idempotency (idempotency keys) is explicitly deferred and tracked in LFXV2-2665. Only the campaign and line item are created with
`entity_status=PAUSED`; the promoted-tweet endpoint does not accept
`entity_status`, so the API creates that association `ACTIVE`. It cannot serve,
though, because the parent line item is paused — delivery is gated by the paused
line item, not by the association's own status.

Per the X Ads v12 contract, create endpoints take their parameters as URL query
parameters (not a JSON body); the client folds those params into the OAuth
signature base string. Flight dates (`start_time`/`end_time`, ISO8601 UTC) are
sent only on the line-item create, where they are required; the campaign
endpoint does not accept them in v12, so the campaign create omits them. Dates
are validated for shape, real-calendar validity (`time.Parse`), ORDER (end after
start), AND for a future start: the start's emitted midnight-UTC instant must be at
least `minStartLead` (5m) ahead of now, so today (or a start only moments ahead) is
rejected before any mutating call — otherwise the multi-request create flow could cross
the start time and X would reject the now-past line-item start, leaving an orphan.
Budget is likewise validated pre-create (positive, ≤ 1e9, rounds to ≥ 1 micro-unit). The client paces writes toward X's 1-req/sec limit on the
CLIENT INSTANCE, not per call site: `pace` reserves the next write slot under the
client's own `writeMu`, so concurrent callers sharing an instance are spaced
`writeDelay` apart in aggregate rather than each sleeping in parallel and then
issuing together. That is why the dispatch layer shares one client per connection
(see `internal-dispatch`) — sharing is the precondition for the budget being
enforceable, not a hazard. Reads are not paced and stay fully concurrent, since they do
not spend the write budget — X's read endpoints have their own limit windows, which
the shared 429 backoff covers for GETs exactly as it does for writes. A retried WRITE
takes a fresh slot, because the 429 backoff is not itself a reservation. A cancellation
OBSERVED BEFORE ADMISSION reserves nothing — not the stronger "a cancelled caller
reserves nothing", which `pace` does not provide: cancellation can land between the
final `ctx.Err()` check and the `nextWrite` update.
`TestPaceCancelAtWaitExpiryReservesNothing` documents and tolerates that residual
window, measured near 0.5% against roughly 98% unfixed.

Because `pace` RESERVES a slot rather than holding one open, nothing unpaced may
sit between the reservation and the write it spaces out. The tweet-authoring arm
of `CreateCampaign` used to reserve and then run the `promotable_users` GET, so a
concurrent writer sharing the client could reserve and issue inside that window
and the two writes landed together — rebuilding the burst the pacer exists to
prevent. Reads are unpaced and cost nothing to move, so `resolvePromotableUser`
runs first and `pace` is called immediately before `createNullcastTweet`.
`TestCreateCampaign_PacesImmediatelyBeforeAuthoring` pins the ORDER of the three
observed events (GET -> admit -> POST) rather than an elapsed duration, which
would pass for the wrong reason on a slow machine.

`onPaceWait` is `onAdmit`'s sibling and complement: `onAdmit` fires once a wait is
over, so it cannot be used to act DURING the window a caller is parked in, and
that window is where a cancellation between two writes has to land. Firing a
test-only hook just before `sleepCtx` (under `writeMu`) turns that window into an
ordinary synchronous callback, which is how
`TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID` now lands its
cancel. It previously bet a 50ms sleep against a 500ms write delay; the bet was a
good one and still a bet, and every way of losing it landed on one of that test's
two silent-pass modes. Both hooks are nil in production.

The bound is per client instance, which is narrower than per ACCOUNT. Two clients
for one X ad account still pace independently, and three ordinary things produce
them: separate replicas; two PROJECTS whose connections point at the same ad
account (the client cache is keyed by project + connection row); and cache
replacement by rotation, TTL or LRU while an in-flight caller still holds its
predecessor. So this removes the common case — a burst of concurrent dispatches
for one project — and leaves the residue to the 429 backoff. A limiter keyed by ad
account, with a lifetime independent of the client cache, plus cross-replica
coordination, is tracked in LFXV2-2665; operators must not rely on this client for
account-wide rate limiting. When the account limit is hit anyway, 429s are
retried with backoff bounded by `Retry-After` / `X-Rate-Limit-Reset` — but only
for calls the caller declared IDEMPOTENT.

Retry eligibility is an explicit `idempotent bool` parameter threaded through
`request` -> `createRequest` -> `doRequest` -> `doRequestAbs`, never inferred from
the HTTP method, and a non-idempotent call takes the retry-exhausted exit on its
FIRST 429 (`attempt >= retryMax || !idempotent`). The method cannot carry this: X
answers a 429 at OR AFTER committing the write it throttled, so "POST" says
nothing about whether re-issuing is safe. What decides it is whether the endpoint
converges on a repeat, and three of this client's four creates do — campaigns and
line items are found-or-created by name, a repeated `promoted_tweets` POST comes
back `DUPLICATE_PROMOTABLE_ENTITY`. Tweet authoring is the one that does not: a
tweet has no name to find it by and no idempotency key, so a retried 429 publishes
a SECOND tweet under the LF handle, and the request layer would have done it twice
more before `createNullcastTweet` returned. It therefore passes `false`, alone in
this package. The throttle still surfaces as an `*apiError`, which
`createOutcomeAmbiguous` classifies as ambiguous, so the caller renders UNCONFIRMED
and asks the operator to verify in X Ads Manager — a human check before a second
publish is the only safe form a retry of that call can take. (This is the same
convention the googleads client uses for its own POST `:search` / POST `:mutate`
split.) If the caller's
context expires DURING that backoff sleep, the client returns the 429 as a typed
`apiError` (with the cancellation cause attached via `Unwrap`) rather than a bare
`ctx.Err()`: the throttle already happened, and a mutating 429 is ambiguous, so erasing
it would report "not modified" for a write that may have applied. This is reachable —
`maxRetryWait` (90s) exceeds the orchestrator's `toggleCallTimeout` (45s), so a
server-declared `Retry-After` in between is accepted for sleeping and then interrupted.
Both 429 branches — the retry and the exhaustion — hand the response to `drainAndClose`,
which discards up to `maxResponseBody` before closing. `net/http` only returns a
connection to the idle pool after its body reaches EOF and is closed, so closing a 429's
unread error envelope would make the very next retry reopen TCP and TLS.
Redirect
following is force-disabled (a shared `noFollow` `CheckRedirect` policy). For a
`WithHTTPClient`-supplied client, `NewClient` builds a FRESH `*http.Client`
carrying the caller's reusable exported fields (`Transport`, `Jar`, `Timeout`) with
`CheckRedirect: noFollow`, rather than value-copying the caller's client (an
`http.Client` must not be copied after first use). So a 3xx is surfaced rather than
followed — important with OAuth 1.0a, where a followed redirect would resend a
request signed for the original URL to a different one.
A non-2xx surfaces a typed `apiError`. Its `Error()` renders only method/path/
status — the raw body is NOT echoed, and neither are X's machine-readable error
codes, so a signed URL / destination secret (which an untrusted body could place
even inside `errors[].code`) can't leak into a persisted Step. The codes are
retained on the struct solely for internal classification via `hasErrorCode`
(e.g. matching `DUPLICATE_PROMOTABLE_ENTITY`), and `parseErrorCodes` bounds what
it keeps (drops over-long values, caps the count). This mirrors the reddit
client, whose `apiError` likewise retains `Body` for classification but never
surfaces it. An ambiguous
transport/read/decode failure surfaces a `transportError`, and a pre-connect dial
failure surfaces a `preSendError`. BOTH render URL-free: `httpClient.Do` returns a
`*url.Error` whose `%v`/`String()` embeds the full request URL (and X puts create
parameters in the query string), so a naive `%w`/`%v` of that error would leak the
URL into the copied `PromotedTweetWarning` and persisted Steps. Each type's
`Error()` runs the cause through `safeTransportCause`, which peels EVERY nested
`*url.Error` layer down to the URL-free underlying cause (timeout/EOF/ECONNREFUSED);
`Unwrap()` retains the real cause so `errors.Is`/`errors.As` (incl.
`isPreSendDialError`) still match. `preSendError` is DEFINITE (request never sent →
not applied), distinct from the ambiguous `transportError`.
`createOutcomeAmbiguous` treats a mutating 3xx/5xx (and transport error) as
UNCONFIRMED so a create that may have committed is not blind-retried into a
duplicate; a `preSendError` is neither, so it stays a definite "not applied".

## Authoring a promoted tweet (brief -> servable ad)

`CreateCampaign` can author the ad's creative itself — text only — so a caller can
go from a brief to a servable X ad without first hand-composing a tweet in X Ads
Manager, closing the gap that used to make X the one platform of the six this
service dispatches to that always needed a manual step. An explicit `TweetID`
always wins over `TweetText`: if both are supplied, the given tweet is promoted
as-is and the text is ignored (recorded as a step). `TweetText` is only consulted
— and only validated — when `TweetID` is empty; an unused, possibly-malformed
`TweetText` alongside a valid `TweetID` must not fail an otherwise-good campaign.

The endpoint is `POST accounts/:account_id/tweet` — **on the Ads API host itself**,
not `api.x.com/2/tweets`. That is the key simplification: it is account-scoped,
signed by the same OAuth 1.0a header, and takes query parameters like every other
create call here, so authoring slots into the existing `createRequest` with no new
host, no new auth mode, and no JSON body support. The client always sends
`nullcast=true` **explicitly** — it is X's documented default, but the one failure
mode that matters (a real tweet landing on the Linux Foundation's public timeline)
is exactly the one an implicit default can't be tested for, so the literal
parameter plus a test assertion on it stands in for that guarantee. `nullcast` is
what keeps the tweet *promoted-only*: it is never visible on the public timeline
or to followers, and a promoted-only tweet needs only `TWEET_COMPOSER`
permission — only an *organic* (`nullcast=false`) tweet needs the full promotable
user, and this client never sends `nullcast=false`.

`resolvePromotableUser` decides which handle authors the tweet via
`GET accounts/:account_id/promotable_users`. It fails closed rather than
guessing: a pinned `AsUserID` not present in that list is refused, zero
candidates is refused, exactly one candidate is used automatically, and several
candidates with none pinned is refused so the caller can pin one. This mirrors
the account/funding-instrument validation's "never silently pick" posture
elsewhere in this client.

Its refusals carry the COUNT of candidates and never the user ids themselves.
These strings do not stop at a log line: they become the campaign result's
`PromotedTweetWarning` and a `Steps` entry, both persisted and rendered, so
naming the candidates published the account's promotable X handles to every
reader of that campaign. The count is the part that makes the message actionable
("there is more than one, pin one"); the ids only ever needed to be readable in X
Ads Manager, where whoever is about to set `asUserId` is already looking.

It PAGINATES the list, bounded by `maxListPages` with cursor dedup, as
`findByName` and `ListAdAccounts` do. Reading only page one made two silent
errors: a pinned user on a later page was reported as not promotable at all, and
the single-candidate shortcut fired on an account whose later pages held more —
auto-picking an author the caller never chose, which is precisely the "never
silently pick" guarantee above. A pinned id short-circuits the walk the moment it
is seen; auto-resolution must reach the end before it may conclude.

Two different things can leave the walk unable to conclude, and both are checked
before any of those conclusions is drawn.

Reaching `maxListPages` with a cursor still outstanding is NOT reaching the end,
and the walk records which of the two happened. Falling out of the loop otherwise
looks identical to finishing it, and every conclusion below the loop is a claim
about the whole list: "not among them", "none at all", and above all the
single-candidate auto-pick, which on a truncated list picks an author from a set
the caller was never shown. `findByName` refuses on exactly this footing rather
than reporting a not-found it cannot stand behind, and this walk now does too.

The second is a page that ended the walk without saying the list ended. The
`cursorUnknowable` policy here — end the walk rather than fail it — is
deliberate and unchanged, because the function read page one alone before, so
failing on a response shape that works today would break live accounts. But
ending the WALK is not the same as confirming the LIST, and every conclusion
below the loop is a claim about the list. So the discriminator `findByName`
already uses applies here too, and for the same reason: PAGE FULLNESS. X
documents that "if less than count entities are returned in the current page of
the result set, the next_cursor value will be null", so a SHORT page is
conclusively the last one on its own evidence and needs no cursor — which is
what keeps every ordinary small account resolving. A FULL page OWES a cursor,
so a full page with an unknowable one leaves the list unconfirmed and the walk
refuses. `count` is requested explicitly for exactly this reason: under X's
default page size, whether a body is short or full depends on a number this
client never saw, which is not evidence anything may be concluded from.

Its page cursors stay on the WIRE URL and off the error path, via `requestPage`.
A cursor is opaque text decoded out of an upstream response body, and
`doRequestAbs` records its `logPath` into every `apiError`, `transportError` and
`preSendError` — errors that on this path are rendered into `PromotedTweetWarning`
and a persisted `Steps` entry. Folding the cursor into the path therefore wrote
upstream response text into the campaign record, which
`platform-error-must-not-carry-untrusted-or-credential-text` forbids. The
repeated-cursor refusal names no cursor either. `ListAdAccounts` already passed
"the bare collection path, never reqURL" for this reason; `requestPage` is what
lets the two account-scoped walks (`findByName` and this one) do the same.

It differs from `ListAdAccounts` on ONE point, deliberately: `cursorUnknowable`
ENDS the walk here rather than failing it. `ListAdAccounts` refuses to conclude
from a possibly-truncated set because its whole job is enumeration. This function
paginated not at all until now, so every response shape that reaches a decision
today did so from page one alone; turning an absent or empty `next_cursor` into a
hard error would break live accounts to fix a case that cannot be worse than the
status quo. What it reads is a superset of what it read before, and its failure
modes stay the ones the caller already degrades on.

`composeTweetText` builds the actual text sent to X: it appends the destination
URL (the real, non-display counterpart of the manual workflow's
`displayTwitterUtmURL`, built by `buildTwitterUTMURL`) if the caller's text
doesn't already embed it, then validates the composed text against X's
280-character cap via `weightedTweetLen`. That helper SCANS the composed text
for URLs and counts each at X's fixed t.co weight (23 characters) rather than
its raw length — X always wraps posted URLs to a t.co link, so a raw-rune count
would wrongly reject perfectly valid copy carrying a 120+ character UTM'd
registration URL. It scans rather than being handed the one URL the composer
appended, because a caller's own text may already embed that URL (the append is
skipped then) or carry others of its own; weighting only the appended URL would
reject exactly the copy X would accept. This validation runs in the up-front
pre-create block, before any mutating call, like every other CreateCampaign
input check.

Everything that is NOT a URL is counted by twitter-text WEIGHT, not by runes.
X's cap of 280 is a weighted budget: the weight-1 ranges are `[0,4351]`,
`[8192,8205]`, `[8208,8223]` and `[8242,8247]` — Latin, Greek, Cyrillic, Hebrew,
Arabic and common punctuation — and every other rune costs 2. A rune count
therefore under-counted a CJK tweet by half, and 280 CJK characters were accepted
here and rejected by X.

The two directions of error are not symmetric, and that asymmetry decides the
emoji handling. An UNDER-count sends copy X refuses: one wasted round trip and an
operator-facing error. An OVER-count invents a rejection of copy X would have
accepted, which no retry fixes and no error explains. So `weightedRunLen`
collapses an emoji presentation sequence into ONE 2-weight cluster
(`emojiClusterLen`) rather than charging 2 per codepoint: a skin-tone modifier, a
ZWJ family, a keycap and a country flag each cost 2 in total, where a naive
per-rune pass would have charged a family sequence 14. A BMP codepoint starts a
cluster only when it ASKS to be an emoji, so a bare `©` stays weight 1 and `©️`
is one 2-weight cluster — but U+FE0F is not the only way it asks. A skin-tone
modifier or an enclosing keycap directly after a BMP base is itself the request:
`✊🏽` is U+270A followed by U+1F3FD with NO variation selector between them, and
`1⃣` is a digit followed by U+20E3. Requiring U+FE0F saw neither sequence and
charged 2 per codepoint — 4 for a fist X charges 2 for — which is the OVER-count
direction this whole paragraph exists to prevent. Reading a modifier as a request
cannot err the other way: a modifier after a base that is not really an emoji is
malformed text, and folding it into one cluster charges 2 where the per-rune pass
charged 3, still downward.

The count is taken over the NFC-normalised text, as twitter-text does, because X
weighs the normalised form and a guard is only worth having if it counts what X
counts. A decomposed `é` (U+0065 U+0301) is two runes here and one character to
X. Normalising is conservative by construction — NFC composition never lengthens
a string in runes — and it is used for COUNTING ONLY: the text published is the
caller's own bytes, because silently rewriting an operator's copy is not this
function's business.

A second, much looser cap bounds the RAW size of the composed text
(`maxTweetRawBytes`, 8 KiB). The weighted cap is no bound on raw size at all —
a URL weighs a fixed 23 however long it really is — so one multi-kilobyte link
passed validation and was then percent-encoded into the tweet-create request
URI, where it is rejected as an oversized URI AFTER the campaign and line item
exist. It is set far above any real tweet on purpose: the same asymmetry applies,
so a bound that exists to catch an absurd input must not be tight enough to
refuse copy X would accept. 280 weighted characters of four-byte runes is 1120
bytes, so no legitimate brief comes near 8 KiB.

The same asymmetry decides where a URL run ENDS. `tweetURLRe` is
`(?i)\bhttps?://\S+`: case-insensitive because RFC 3986 §3.1 makes the scheme
case-insensitive and `HTTPS://…` is a link X wraps to t.co like any other, where
a case-sensitive match charged it its raw length and invented a rejection. `\S+`
then runs to whitespace, so a link at the end of a sentence swallows the period
that follows it — so `trimTweetURLPunct` peels trailing `.,;:!?'"` and any
closing bracket with no opener inside the run. Both directions of that are real:
the punctuation counted inside the t.co weight under-counts, and the same run
handed to `url.Parse` for credential screening is not the URL that will be
fetched. The trimmed link is a PREFIX of the run, so it still locates at the
run's offset in `weightedTweetLen`; advancing past only its length leaves the
punctuation in the remaining text to be weighted as the prose it is.

This is the DELIBERATE OPPOSITE of `sanitizeSnapshotText` in
`internal/dispatch/creds.go`, which takes the greedy run and does not trim, and
the two must not be "unified". There, over-reach fails SAFE — a period swept into
a sanitised snapshot costs nothing, and trimming could leave credential text
outside the run. Here over-reach fails UNSAFE in both directions, because the run
is used to count a budget and to parse a URL.

`rejectCredentialQueryParams` screens a single URL's query, and
`rejectCredentialQueryParamsInText` runs it over every URL in the COMPOSED tweet
text — after `composeTweetText`, before anything mutates. Screening the composed
artifact rather than the `RegistrationURL` input is the point: what is checked is
then byte-for-byte what is published, it covers links the caller put in their own
copy (which the input-level check never saw and which `TweetText` carries
verbatim), and a future change to how the text is composed cannot route a URL
around the gate. It is ONLY on this path. Everywhere else that URL is a click destination
whose query a server reads; here `composeTweetText` puts it in the tweet body,
where it is world-readable forever — and a registration link pasted out of a
logged-in browser carries whatever that session put in it. The check is a
DENYLIST, not an allowlist, and that is the deliberate call: LF event pages carry
real routing and attribution parameters nobody can enumerate in advance, so an
allowlist would refuse working briefs to protect against nothing, while a
denylist refuses only keys that are credentials under any reading.

`isCredentialQueryKey` is a PREDICATE, not a name list, because credential
parameter names COMPOSE: `secret_token`, `access_key`, `auth_key`,
`oauth_token_secret`, `x_request_signature` are all obvious credentials and all
absent from any set someone thought was finished. Enumeration does not converge.
Keys are normalised first — `-`, `_` and `.` removed, case folded — then matched
in three tiers: the exact set for spellings that carry no fragment
(`jwt`, `password`, `sessionid`); a fragment list of words that are unambiguous
as a COMPONENT of a compound (`token`, `secret`, `credential`, `signature`,
`hmac`, `jwt`, `bearer`, `oauth`, `authorization`, `assertion`); and a "…key"
SUFFIX rule minus an explicit benign set (`monkey`, `donkey`, `turkey`,
`whiskey`, `jockey`, …). The tiers exist because `key`, `auth`, `sig`, `pass` and
`session` are exactly the words that CANNOT be fragments — `keyword`, `oauth`
inside nothing, `design`, `bypass`, `passenger` — so they stay exact-only, and
`key` gets the suffix rule instead, which is the position where it really is one.
`sessionid` is the one compound promoted INTO the fragment tier: the two standard
spellings of a session cookie carried in a URL, `JSESSIONID` and
`ASP.NET_SessionId`, normalise to names the exact set never had, and unlike bare
`session` the full `sessionid` collides with no routing parameter. `code` and
`pin` are weighed and excluded on purpose — a discount code is the common meaning
on a registration link.

The query is parsed with `url.ParseQuery` and the gate fails CLOSED on its error,
NOT with `u.Query()`, which discards that error and returns whatever pairs it
decoded. A query Go refuses to decode — an unescaped `;` separator, a bad escape
— therefore arrived as an empty map, and the screen cleared a URL whose
parameters it had never read.

The error names the offending KEY and never its value — but only when that key is
really a name. The reasoning holds because the secret is the VALUE, which the
parser holds separately and which is never rendered; it fails entirely for a URL
ending in a bare `?eyJhbGciOi…`, which has no `=` at all, so the whole token lands
in the key position. Bounding that is not redacting it: a credential prefix is
still credential material. So the two cases are split.
`queryKeysWrittenWithAValue` re-reads the raw query — `url.ParseQuery` cannot
answer this, giving the empty string for the value of both `?token=` and `?token`
— and only a key the caller actually wrote as `name=value` is rendered, through
`safeQueryKeyForError`, which strips control characters and truncates by rune
before the name reaches an error that is persisted and logged. A bare component
is named as a CATEGORY and never echoed. The check runs in the up-front block so
the refusal costs a corrected brief rather than an orphaned campaign.

USERINFO is refused outright, before the query is read at all.
`validateRegistrationURL` already rejects `https://user:password@host/…`, but a
link the caller pasted into their own copy never passes through that validator,
and this gate read only query keys — so an embedded credential in a URL with no
query string at all was published verbatim. Neither half of the userinfo is named
in the refusal; the URL is redacted, which is what locates the offending link
without the error becoming the leak it exists to prevent.

`buildTwitterUTMURL` diverges from `displayTwitterUtmURL` in one way that
matters: it preserves the registration URL's own pre-existing query parameters
verbatim alongside the UTM ones, because this URL is the ad's actual click
destination and those parameters are frequently what routes the visitor.
The display form strips them because its job is safe persistence, not routing.
That query is parsed with `url.ParseQuery` and fails CLOSED on its error, for the
same reason the credential screen does and with more at stake: here the pairs are
not merely invisible, they are OVERWRITTEN, because the re-encoded query replaces
`RawQuery` wholesale. A registration URL carrying `?ref=partner;session_token=…`
would have lost its routing parameters silently, sent real click traffic to the
wrong page, and reached the credential screen with nothing left to object to.
The fragment is dropped in both — it never reaches a server, so it cannot carry
attribution and only widens what gets published.

Authoring happens at **Step 4**, immediately before the `promoted_tweets` POST —
deliberately NOT alongside the campaign/line-item creation earlier in the flow,
unlike the reddit client's equivalent (which authors its post *before* the paid
campaign, at "Step 1.5", to avoid orphaning a paid resource on an authoring
failure). That trade-off inverts here: X's campaign and line item are created
`PAUSED` and cost nothing, so there is no paid resource to orphan, while a
published tweet under the LF handle *is* the expensive artifact — authoring
right before promoting minimizes the window a stray tweet could sit unattached.

Authoring's outcome is classified with the SAME house rule the `promoted_tweets`
POST already follows (`createOutcomeAmbiguous` checked BEFORE any status-code
branch — see commit `88224984`, which fixed a real bug from getting this order
backwards): an ambiguous failure (mutating 3xx/5xx, or a transport error) means
the tweet MAY have been published, so the warning says to verify in X Ads
Manager and delete any stray tweet before retrying, never "safe to retry"; a
definite 4xx or pre-send failure means nothing was published, so it IS safe to
compose manually or retry; and a 2xx with no `data.id` (a malformed success) is
treated the same as the ambiguous case, not as a clean win, for the same reason
the `promoted_tweets` 2xx-no-id case is: a response X returned successfully but
whose id this client couldn't read is not proof nothing happened. ("No id" means
no `id_str` — see `extractTweetID` below, which reads X's string-typed field
rather than the numeric `id` every other endpoint on this client uses.) All of
this stays non-fatal exactly like the rest of Step 4 — only a `pace(ctx)`
cancellation returns an error.

That `createOutcomeAmbiguous` split classifies the authoring POST's own outcome.
It does NOT gate the cancellation path: a `pace(ctx)` abort returns a non-nil
partial result unconditionally, whatever the classification would have been, so
the orchestrator's claim is retained in every case rather than only the ambiguous
one.

That partial result carries the authored tweet's id. `authoredTweetID` is
declared ABOVE the `partialResult` closure rather than beside the other Step 4
locals, because a published tweet is the only irreversible artifact this flow
creates: the campaign and line item are `PAUSED` and are found-or-created by
name on a retry, but a tweet is not, and it sits under the LF handle until
somebody deletes it. A cancellation between authoring and the `promoted_tweets`
POST therefore returns `AuthoredTweetID` populated and an error naming the
tweet as PUBLISHED-but-unpromoted, instead of reporting `""` for a tweet that
provably exists and leaving the prose Steps entry as its only trace.

The authoring response's id is read by `extractTweetID`, not the generic
`extractID` every other endpoint on this client uses: `accounts/:id/tweet`
returns a legacy v1.1-shaped tweet object whose `id` is a JSON **number**
(large enough to lose precision as a float64), unlike every other Ads API
entity's string `id` — `extractID`'s string-typed field silently fails to
unmarshal against it and returns `""`, indistinguishable from a genuinely
missing id. `extractTweetID` instead reads `id_str`, the same value as X's own
string-typed escape hatch, so a successfully authored tweet is no longer
misclassified as the malformed-success (2xx-no-id) case above.

What it extracts is then held to the SAME shape an explicit `TweetID` must
satisfy — `tweetIDRe` plus the int64 range check — because it is used the same
way: promoted via `promoted_tweets`, recorded in `AuthoredTweetID`, persisted
into `Steps` as the id an operator looks up. The caller only tests it for
emptiness, so without that an arbitrary non-numeric string in a 2xx body was
reported as a CONFIRMED authored tweet and then failed at `promoted_tweets`,
after the campaign and line item existed — exactly what validating the explicit
id up front was for. An invalid value returns `""` and takes the
malformed-success path instead, and the rejected value is never echoed: it is
upstream response text.

A successfully authored tweet's id flows into the exact same `tweetID` variable
an explicit `TweetID` would have populated, so it falls through into the
pre-existing `promoted_tweets` POST and its four-way classification unchanged —
there is exactly one promote path, regardless of whether the tweet came from the
caller or was just authored. `CampaignResult.AuthoredTweetID` records the newly
authored tweet's id distinctly from `PromotedTweetID` (the promoted-tweet
association's own id), so a caller can tell "we made a new tweet" from "we
attached tweet X to a line item" even when both succeeded.

Scope is deliberately **text-only**: `media_keys` (images/video) requires a prior
chunked-upload call to a different host this client does not implement, so image
tweets are out of scope for this path — the manual workflow remains the only way
to attach a tweet with media, for now.

## Status toggle

`UpdateCampaignAndChildrenStatus(ctx, campaignID, lineItemID, status)` toggles an existing
campaign between `ACTIVE` and `PAUSED` (X's `entity_status`). Like the create path it PUTs
its parameters as QUERY PARAMS, not a JSON body (the X Ads v12 contract), and it takes the
1-req/sec write pacing.

SCOPE is the campaign + line item ONLY. `CreateCampaign` leaves the promoted-tweet
association ACTIVE (that endpoint does not accept `entity_status`) and the LINE ITEM is X's
delivery gate, so pausing the line item stops serving and re-activating it resumes serving
without the association ever moving. Toggling the promoted tweet would be unnecessary and,
on activate, unable to make an otherwise-paused tree serve.

ORDER: on ACTIVATE the line item flips FIRST and the campaign gate LAST (nothing serves
until the tree is ready); on PAUSE the campaign gate flips FIRST (delivery stops
immediately). An ACTIVATE with a blank line-item id is refused BEFORE any call — the line
item would stay PAUSED and nothing would serve. Pausing needs no line-item id.

The account id and both entity ids are validated with `accountIDRe` BEFORE any request, the
same up-front path-injection guard the create path applies — they interpolate into
`accountURL` and the request path, so a stored id carrying `/`, `?`, or `#` could redirect a
signed PUT to a different account or entity.

OUTCOME CLASSIFICATION: once the first entity has been changed, a failure on the second
returns a `partialCascadeError`, whose `Unconfirmed() bool` reports true — so even a
DEFINITE 4xx on the child is an ambiguous OVERALL outcome (the parent genuinely changed) and
the caller is told to verify rather than "not modified". A failure on the FIRST call mutates
nothing, so a definite 4xx stays definite. The exported `IsOutcomeUnconfirmed` folds this
together with `createOutcomeAmbiguous` for callers across the package boundary (the
dispatcher), mirroring the reddit client's helper of the same name.

## Metrics reads

`GetCampaignMetrics(ctx, campaignID, window)` reads impressions, clicks, and spend metrics for
a campaign from the X Ads synchronous analytics (`stats`) endpoint. It is a **LIVE READ ONLY**
— never persisted, no async sweeper. Window is a predefined date-range literal (`WindowYesterday`,
`WindowToday`, or `WindowLast7Days`); an unsupported window returns the typed `ErrUnsupportedWindow`
sentinel (discriminable via `errors.Is`, not string-matching) rather than silently truncating or
averaging. Campaign ID validation similarly returns the typed `ErrInvalidCampaignID` sentinel.

**CRITICAL DESIGN CONSTRAINT: X Ads API stats endpoint caps queryable date ranges at 7 days
per request.** Supported windows: `WindowYesterday` (1 day), `WindowToday` (1 day), and `WindowLast7Days`
(7 days). Any request for a longer window (`LAST_14_DAYS`, `LAST_30_DAYS`, `THIS_MONTH`,
`LAST_MONTH`) is REJECTED with `ErrUnsupportedWindow` — NOT silently truncated, averaged, or
extrapolated. This is a permanent platform constraint documented in the knowledge base.

`CampaignResult` carries `AccountID` (LFXV2-3050), stamped from `c.account.AccountID` at every
construction site including the partial-result paths, so the dispatcher's provenance guard can
refuse a toggle or metrics read whose connection has since been re-pointed to another ad account.
The field is UNTAGGED like the rest of this struct, so the persisted key is the Go field name.
X has NO recoverable fallback for it: `TwitterURL` is the bare `https://ads.x.com` constant and
never carried an account, so a pre-existing row records no provenance and is treated as
"unknown, proceed". See `internal-dispatch.md` for the guard itself.

**The stats endpoint is NOT nested under `/accounts/{id}` the way every other endpoint this
client calls is** — it's `{base}/{version}/stats/accounts/{id}` (account id trailing, not
leading). `doRequest` always builds `accountURL()+path` (`/accounts/{id}/{path}`), so this
method calls the new `statsURL()` + `doRequestAbs` directly instead, bypassing that prefixing.
`doRequestAbs` is `doRequest`'s retry/OAuth core extracted so a caller can target a
non-account-scoped URL while still getting the same 429 exponential-backoff/OAuth1-signing
behavior; `doRequest` itself is now a thin wrapper that builds `accountURL()+path` and
delegates to it.

The response is `{"data":[{"id":"…","id_data":[{"metrics":{"impressions":[…],"clicks":[…],
"billed_charge_local_micro":[…]}}]}]}` (Rest.li-flavored: each metric is an array indexed by
time bucket; `granularity=TOTAL` in the request means exactly one bucket). `billed_charge_local_micro`
is already in micro-currency units — no USD-decimal parse/round conversion, unlike platforms
that report spend as a decimal-USD string. X omits a metric field entirely (not a zero) when
there's no activity for it — a nil/missing array is read as 0, which is real "no data", not a
decode failure. **UNVERIFIED ASSUMPTION**: the required `metric_groups=ENGAGEMENT,BILLING` and
`placement=ALL_ON_TWITTER` params and this response shape follow the documented X Ads v12
`stats/accounts/:account_id` contract, but have not been verified against a live X Ads account.
CTR is computed as clicks/impressions (0 when
impressions is 0, never dividing by zero). Campaigns with zero activity in the window return
zero-value metrics (not an error).

## Ad-account discovery

`ListAdAccounts` (`accounts.go`, LFXV2-3319) enumerates every X Ads account the client's
OAuth 1.0a user context can reach, so a connection that holds only credentials — or one
being re-pointed at a different account — can ask which accounts are available. The request
is `GET {base}/{version}/accounts`, the COLLECTION form of the `/accounts/{id}` resource
every other call in this client is nested under; X documents it as "a listing of advertising
accounts that the current user has access to". No account id appears anywhere in the path or
query, which is what makes it callable before an account has been chosen.

**It is the one call that must NOT go through `doRequest`.** That helper roots every path at
`accountURL()` — `/accounts/{id}` — so routing discovery through it would ask about a single
account while returning a plausible list. It uses `doRequestAbs` instead, the same escape
hatch the stats endpoint uses, which applies the identical OAuth1 signing, redirect policy,
bounded read and three-way error classification. `logPath` is the bare `accounts` label, never
the request URL, because the URL carries the cursor query and `apiError`/`transportError`
render their `Path` into strings that are persisted into a campaign's Steps.

**Optional narrowing parameters are all deliberately unsent.** `account_ids` scopes to a
caller-supplied subset, `q` prefix-matches on name, `sort_by` reorders; sending any would
silently narrow the picker to whatever the code guessed, and a caller cannot tell a narrowed
list from a complete one. `with_deleted` is unsent too, taking X's documented default of
`false` — but the `deleted` flag is still carried per row rather than assumed, so a flagged
row cannot pass as live. `count=1000` is X's documented maximum (min 1, max 1000, default
200); requesting the maximum raises how many accounts the walk can enumerate at all, since
the page cap bounds the total.

**Empty must stay distinguishable from failure, and X does not document the zero-account
case.** The choice made is the fail-loud one: an empty, non-nil slice with a nil error is
returned ONLY when X sent `"data":[]` together with the documented exhaustion `null` cursor — a
body that affirmatively says "here is the set, and it is empty". Anything less is an error, so an empty
answer always means X said the set was empty, never that the call failed.

Two absence guards implement that, and both are subtler than they look:

* **`"data":null` is not nil.** `encoding/json` stores the four bytes `null` in a
  `json.RawMessage`, so an absent `data` and an explicit null cannot be told apart by a nil
  check on the raw field — and `null` then unmarshals into a nil element slice, reporting a
  healthy zero accounts. The guard therefore tests the DECODED slice for nil after decoding:
  a present `[]` yields a non-nil empty slice, while both absent and null leave it nil. Every
  walk that reads `data` makes this check, for the same reason it reads the cursor carefully —
  a body that reported no result set must not become a claim that the thing does not exist. The
  name lookup answers a false "not found" with a create POST, so there it is a duplicate paid
  campaign; the accounts walk would present a truncated picker as a complete one.
* **Only the documented null is exhaustion.** X documents termination as an explicit null
  ("If less than `count` entities are returned in the current page of the result set, the
  `next_cursor` value will be `null`"). A plain string field collapses null, absent and empty
  onto `""`, so `apiResponse` carries two extra bits set by a custom `UnmarshalJSON`:
  `NextCursorPresent` (did the KEY appear) and `NextCursorNull` (did it hold a literal `null`,
  read from the RAW bytes, because decoding is exactly what erases that distinction).
* **One classifier, consulted by every cursor walk.** `cursorVerdict` turns those bits into
  three outcomes — a usable cursor, the documented exhaustion null, or *unknowable* (the key
  absent, or present but empty). Both walks in the package route through it, so they cannot
  disagree about what a cursor means. They had diverged exactly once, in the way a shared
  reader prevents: the accounts walk rejected an empty cursor while the name lookup tested
  only key-presence, so `"next_cursor":""` was present, skipped the guard, and reported a
  confident "not found" that the caller answers with a create POST — duplicating a live
  campaign.
* **What each walk does with *unknowable* differs, and that is contract, not cursor reading.**
  `ListAdAccounts` owes EVERY account or an error, so it can never accept it. `findByName` may
  still conclude from a SHORT page, which X's rule makes conclusively the last one on its own
  evidence; only a FULL page leaves it unknowable whether another page holds the name. Neither
  can lean on a page cap to cover this: the cap is reachable only while a usable cursor keeps
  arriving, which is precisely the case an unknowable cursor is not.

  This is the same absent-vs-null-vs-empty distinction the `data` guard draws, arriving
  through the pagination door.

A walk that cannot be completed is an ERROR, never a short list. Anything that leaves the set
unconfirmed — a cursor `cursorVerdict` calls unknowable, a repeated cursor, the
`adAccountMaxPages` (20) cap, a `data` that is not an array, or a row whose id fails
`accountIDRe` **or exceeds `maxAccountIDLen` (64)** — returns nil rather than what was
collected. The id check reuses the SAME regexp every account-scoped path validates a
configured id against, so an account this walk offers must be one the client will later accept
— and the LENGTH bound is the other half of that same contract:
`design/connection.go` caps `twitter-ads-connection-config.account_id` at `MaxLength(64)` as
well as `Pattern(^[A-Za-z0-9]+$)`, and Goa enforces both at bind time. Checking only the
charset advertised a 65+ character alphanumeric id as ready to store that would then be
rejected as a 422 every time it was selected — a permanently dead entry in the picker,
indistinguishable from a live one. The bound is applied at the discovery site rather than by
tightening `accountIDRe`, which also guards the create/metrics/toggle paths where an already
stored id's length is not theirs to re-litigate. A bad row fails the WHOLE walk rather than
being skipped, because a response shape that far from the documented one means the rest of it
is not trustworthy either — and a partial list looks complete.

The row's id is validated **RAW — it is deliberately not trimmed** (LFXV2-3319 follow-up). An
account id is an opaque upstream token, so trimming `" acct1 "` does not clean the row up, it
INVENTS the different id `acct1` and offers it as one X sent — binding a connection to an id
we never saw. `accountIDRe` is anchored and admits no whitespace, so a padded id fails the walk
on its own, exactly as the enumerated policy above says a non-alphanumeric id must; repairing
it silently would exempt the one malformation that happens to be easy to repair. The page
cursor is left untrimmed for the same reason. `Name` and `Timezone` ARE trimmed — they are
display labels, not identifiers, and nothing binds to them.

**Unusable accounts are RETURNED, labelled, never filtered.** Accounts under review or
rejected come back carrying their reason; dropping them would answer "your credential reaches
no ad accounts" about an account sitting right there. Deleted rows are the one case NOT
promised: `with_deleted` is unsent, so X's documented default of `false` normally excludes
them upstream — the per-row `deleted` flag is honoured defensively so a row that arrives
flagged anyway is labelled rather than passing as live, but the walk cannot make a deleted
account discoverable. `approvalStatusLabels`
is an ALLOW-LIST of KNOWN-BAD values, because **X publishes no complete `approval_status`
enum** — its reference shows only `ACCEPTED`. An unrecognized or absent status therefore yields
`""` from `ApprovalLabel()`, which is not a claim the account is fine, only that this package
has nothing to say; the raw value still travels to the caller in `Status`.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` twitter adapter (see [internal/dispatch](internal-dispatch.md))
interprets an OAuth1 4-tuple (consumer key/secret + access token/secret); AccountConfig
comes from AccountID + `funding_instrument_id`. Budget (`budgetAmount`) is in the
ACCOUNT's currency (no FX). It surfaces a `Reused` reuse/config-drift flag and classifies
an exhausted mutating 429 as UNCONFIRMED; it validates the destination URL (https/http,
no embedded userinfo) up front. It maps `tweetText`/`asUserId` straight into
`CampaignInput` alongside `tweetId` — see "Authoring a promoted tweet" above
for the client-side precedence and classification rules. A fully authored +
promoted tweet degrades exactly the same way an explicit-`tweetId` run does:
the adapter's degrade check is keyed on `PromotedTweetID` being empty
(regardless of whether the tweet was supplied or just authored), so authoring
success collapses cleanly into the existing `created` vs `created_degraded`
decision without a separate trigger.

`validateTwitterConnection` holds the credential rules
shared by `Dispatch` and `ToggleStatus`, with ONE intentional asymmetry:
`funding_instrument_id` is required only by `Dispatch`. It is a create-time field that
`UpdateCampaignAndChildrenStatus` never puts on the wire, so requiring it in the shared
validator would refuse an otherwise-valid pause. Do not fold that check into
`validateTwitterConnection` — both halves are pinned by tests.

It implements `StatusToggler` with a DIFFERENT cascade shape: scope is the campaign + line
item ONLY. `CreateCampaign` creates both PAUSED but the promoted-tweet association is
created ACTIVE by the API (that endpoint does not accept `entity_status`), and the LINE
ITEM is X's delivery gate — so pausing the line item stops serving and re-activating it
resumes serving without the association ever changing. Toggling the promoted tweet would
be unnecessary and, on activate, unable to make an otherwise-paused tree serve.
`UpdateCampaignAndChildrenStatus` PUTs `entity_status` (query params, not a JSON body, per
the X Ads v12 contract), ordering child-first on ACTIVATE and campaign-gate-first on
PAUSE. An ACTIVATE with an unknown line-item id is refused as `ErrCampaignNotProvisioned`
(a 409) before any call.

See [internal/platform/twitter](../../../internal/platform/twitter).

## Connection-probe predicates (LFXV2-2665)

`probe.go` exports `ProbeCredentialRejected(err) bool` and `ProbeInconclusive(err) bool` over this
package's own error types. `internal/dispatch` consults them **in that order** for every platform
— `ProbeInconclusive` defaults to `true` for an unrecognised error (an error nobody classified
proves nothing about the credential), so a revoked credential usually satisfies both and only the
order decides whether the operator is told their connection is broken or that the check did not
complete. Neither predicate true is a third outcome: the platform refused a request this service
BUILT, which is a service defect rather than a verdict.

`probe.go` also exports `ProbeNotSent(err) bool`, the third and lowest-stakes member of the
vocabulary: it answers only whether the failure ever left this process, and it changes nothing an
operator sees. `internal/dispatch` has to ask it at the same boundary because the platform error
chain is DROPPED there, so no later layer could tell a provider that answered badly from one that
was never contacted; the answer reaches `Orchestrator.ProbeConnection`'s metrics arm alone, which
keeps a local DNS or dial failure off `campaign_upstream_call_duration_seconds` rather than
charging it to the provider's error rate. Its default runs OPPOSITE to `ProbeInconclusive`'s on
purpose: `false` for an unrecognised error, so an error nobody classified stays on the upstream
series instead of vanishing from it.

The one error it claims beyond a dial failure is a caller that had ALREADY given up:
`Client.doRequestAbs` checks `ctx.Err()` at its entry and returns `errRequestContextAlreadyDone`
wrapped around it, and `ProbeNotSent` matches that marker alongside `preSendError`. X has no token
leg for the sibling clients' `errTokenContextAlreadyDone` to guard, and `doRequestAbs` is the
single path every Ads API call takes, so its entry is where the equivalent check belongs. The
marker is deliberately NOT a `preSendError`: that type names a DIAL failure and exists to strip a
URL out of the cause, and there is no URL and no dial here. Only that entry is marked — a context
error out of `http.Client.Do`, or on a retry attempt after the first, can arrive with bytes
already sent.

There is no token-refresh arm: X uses an OAuth 1.0a four-tuple, signed per request, with no
exchange to fail. The probe reads the account root directly, so its `404`/`401`/`403` are answers
about the configured account rather than about a discovery request.

`401`/`403` are rejections, but the `404` gets a THIRD exported predicate,
`ProbeAccountUnreachable` — this package and Reddit's are the only two that export one, because
only their probes name the configured account IN the request path. The `404` says the credential
was accepted and the account was not found, which sends the operator to a different field than
"X refused your credential" does; and dropping it from the rejection predicate without that arm
would make it match neither, which is the service-defect arm — a typed 500 about a connection
the operator merely needs to repoint. `apiError` is unexported, so the dispatcher cannot make
this call itself; it consumes the predicate and answers `accountNotReachable`.

`ErrAccountNotConfigured` is deliberately outside BOTH predicates, for the reason Reddit's
`ErrInvalidAccountID` is: `VerifyAccount` raises it from this client's own configuration before
anything is sent, so X never looked at the credential. It is still a verdict — a connection
naming no account cannot dispatch — but one the dispatcher authors, next to its
`ErrAccountNotSelected` arm.

This package now carries its own `ErrInvalidAccountID` beside it, and for a sharper reason than
symmetry. `VerifyAccount` checked only that the stored id was non-empty before interpolating it
into the account-scoped path, while `CreateCampaign` applied `accountIDRe` — so a stored
`18ce54d4x5t/promoted_tweets` made the probe GET a DIFFERENT account subresource, and a `2xx`
from that reported the connection healthy on the strength of a request that answered a different
question, one campaign creation would then refuse. `VerifyAccount` applies the charset guard and
`accounts.go`'s length bound before building the path (a stored id held to the same rule as a
discovered one), and the dispatcher answers the sentinel as `accountIDNotUsable` — the
pre-send verdict, not a credential rejection and not the inconclusive default.
`TestVerifyAccountRejectsAnUnusableAccountIDBeforeAnyRequest` asserts the CALL COUNT, because a
test that only checked the error would still pass if the request were made and discarded.
