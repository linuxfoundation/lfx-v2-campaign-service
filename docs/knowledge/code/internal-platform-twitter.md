---
type: "Go Package"
title: "internal/platform/twitter"
description: "X (Twitter) Ads v12 client: OAuth 1.0a signing, ad-account discovery, the campaign -> line_item -> promoted_tweet creation flow, per-client write pacing toward X's 1-write/sec account limit, the account monitor's asynchronous stats-job primitives, a campaign budget read+write (GET then one paced, classified PUT of the daily *_local_micro amount) for the budget writer (LFXV2-2665), and a line-item bid_amount_local_micro write made only when the line item bids MAX per link click (which no campaign this service creates does)."
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
nothing about whether re-issuing is safe. What decides it is whether the SERVER
converges on a repeat, and NONE of this client's four creates does. `promoted_tweets`
was the one that looked like it did, and an earlier revision passed `true` for it: a
repeated POST comes back `DUPLICATE_PROMOTABLE_ENTITY`, which is true and is not
convergence. X returns that same code when the tweet is promoted by a DIFFERENT line
item, so the refusal does not say the association THIS call wanted exists — the client
says so itself a few paragraphs up, and deliberately turns it into a manual-verification
warning rather than success. Retrying therefore converts a transient throttle into a
permanent "verify this by hand" on an association that may well have been made correctly
on the first attempt. Campaign and line item creates look like they qualify — both are found-or-created by name — but that
dedup runs in `CreateCampaign`, ABOVE the retry loop, and a retry inside
`doRequestAbs` re-POSTs without consulting it; X does not dedupe those names
itself, so both writes can be accepted and the account ends up paying for two.
Caller-side convergence is not retry safety. Tweet authoring has neither form: a
tweet has no name to find it by and no idempotency key, so a retried 429 publishes
a SECOND tweet under the LF handle, and the request layer would have done it twice
more before `createNullcastTweet` returned. All four therefore pass
`false`. The throttle still surfaces as an `*apiError`, which
`createOutcomeAmbiguous` classifies as ambiguous, so the caller renders UNCONFIRMED
and asks the operator to verify in X Ads Manager — a human check before a second
publish is the only safe form a retry of that call can take. (This is the same
convention the googleads client uses for its own POST `:search` / POST `:mutate`
split.)

The prose AROUND that rule drifted behind it and has been corrected. `createRequest`'s godoc
read as though tweet authoring were the sole endpoint passing `false`; `createNullcastTweet`'s
own comment still said `promoted_tweets` was declared retry-safe, contradicting its call site
directly; `docs/api-catalog.md` advertised "exponential backoff retry on 429 responses"
unqualified; and the two transport retry tests reached the loop through
`createRequest(…, "campaigns", …, true)` with a comment calling that endpoint found-or-created
by name. The tests were the most expensive of the four, because a test is where a policy claim
is normally checked rather than merely stated — they now enter the loop through `request`, the
GET-only helper that is idempotent by construction, which is the only caller shape production
still has for it. The parameter survives all four creates passing `false` deliberately: what
differs is the REASON each says it, and only tweet authoring's is permanent. If the caller's
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
`Error()` runs the cause through `safeTransportCause`, which is a FIXED VOCABULARY
with a default-deny — `context canceled`, `context deadline exceeded`, `timeout`,
`connection closed`, `connection refused`, `connection reset by peer`,
`network unreachable`, `dns lookup failed`, and `transport failure` for everything
else — mirroring `hubspot.safeCause` and the microsoft client's equivalent, so the
three clients fail the same way under the same threat. It used to peel every nested
`*url.Error` layer and then render whatever remained. The peel is necessary and is
NOT sufficient, which is exactly what `hubspot.safeCause`'s own doc comment says:
`WithHTTPClient` is a supported option, so the innermost cause is CALLER-CONTROLLED
text — a transport can return any error it likes with the signed URL inside it, and
peel-and-render hands that straight into `PromotedTweetWarning` and persisted Steps.
Each named case emits THIS PACKAGE'S OWN string rather than the error's, because a
custom transport's timeout error is still caller-controlled text even where the
timeout classification is trustworthy. `Unwrap()` retains the real cause so
`errors.Is`/`errors.As` (incl. `isPreSendDialError`) still match, which is where a
caller that needs detail should be looking. The retained cause is held in an UNEXPORTED
`err` field on all three types — `apiError`, `transportError`, `preSendError` —
and the lowercase is load-bearing, not style. A clean `Error()` closes only the
channel that renders the struct as a string; reflection- and JSON-based logging
walks EXPORTED fields and never calls `Error()` at all, so an exported `Err`
hands the `*url.Error`'s full request URL back to the first structured logger
that touches one of these. That is the shape
`platform-error-must-not-carry-untrusted-or-credential-text` prescribes: kept for
`Unwrap()`, never exported and never rendered. `preSendError` is DEFINITE (request never sent →
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
`(?i)https?://[^\s<>。、！？，：；]+` — no `\b`, for the reason given under the two
scanners below; the linkification boundary is applied in code by
`findTweetURLRuns`, and the credential screen deliberately does not apply it at
all. Case-insensitive because RFC 3986 §3.1 makes
the scheme case-insensitive and `HTTPS://…` is a link X wraps to t.co like any
other, where a case-sensitive match charged it its raw length and invented a
rejection. The run then goes to whitespace, so a link at the end of a sentence
swallows the period that follows it — so `trimTweetURLPunct` peels trailing
`.,;:!?'"` and any closing bracket with no opener inside the run, walking by RUNE
rather than by byte. Two groups of characters are excluded from the run instead
of trimmed off it, because they do not arrive at the TAIL. `<` and `>` delimit a
bare link in plain text, and CJK sentence punctuation follows a link with no
space in front of it, so `…lfx.dev、そして` has the comma mid-run where a trailing
trim never reaches it and the whole Japanese tail disappeared into the link's
fixed 23. None of those characters is legal unescaped in a URL, so ending the run
at them loses nothing. This is not a hypothetical shape for LF, which runs
KubeCon China and Open Source Summit Japan. Both directions of that are real:
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
in four tiers: the exact set for spellings that carry no fragment
(`jwt`, `password`, `sessionid`); a fragment list of words that are unambiguous
as a COMPONENT of a compound (`token`, `secret`, `credential`, `signature`,
`hmac`, `jwt`, `bearer`, `oauth`, `authorization`, `assertion`); a COMPONENT tier
over the separator-delimited parts of the ORIGINAL key; and a "…key"
SUFFIX rule minus an explicit benign set (`monkey`, `donkey`, `turkey`,
`whiskey`, `jockey`, …). The tiers exist because `key`, `auth`, `sig`, `pass` and
`session` are exactly the words that CANNOT be fragments — `keyword`, `oauth`
inside nothing, `design`, `bypass`, `passenger` — so they stay exact-only, and
`key` gets the suffix rule instead, which is the position where it really is one.
`sessionid` is the one compound promoted INTO the fragment tier: the two standard
spellings of a session cookie carried in a URL, `JSESSIONID` and
`ASP.NET_SessionId`, normalise to names the exact set never had, and unlike bare
`session` the full `sessionid` collides with no routing parameter. `sessid` and
`cookie` joined it for the same reason: `PHPSESSID` normalises to a name that
contains neither `sessionid` nor any exact entry, and a parameter carrying a
cookie under any name is carrying the session itself. `code` and
`pin` are weighed and excluded on purpose — a discount code is the common meaning
on a registration link.

The COMPONENT tier exists because the separator fold that makes the other tiers
work is also what breaks them. `auth_cookie` and `connect.sid` normalise to
`authcookie` and `connectsid` — no exact entry, no listed fragment — and both
cleared the screen. They cannot be repaired by adding fragments, because `auth`
is inside `author` and `sid` is inside `aside`, `subsidy` and `president`, all of
which a registration page genuinely uses. Splitting the ORIGINAL key on `-`, `_`
and `.` restores the boundary that tells them apart: `author` is one component
and does not match, `auth_cookie` is two and does. Only `auth`, `sid`, `pwd` and
`passwd` are in that set. `session`, `pass`, `sig` and `key` stay exact-only even
though they are unambiguous as components elsewhere, because this is an EVENTS
service and `session_title`, `session_track` and `day_pass` are real parameters
on a conference registration page — promoting those would refuse working briefs,
the one cost a denylist exists to avoid.

`key` is the one of those four that got a second tier rather than staying purely
exact-only, and the reason is that the suffix rule only fires on a name that ENDS
in `key`. `api_key_LEAK` and `access_key_AKIAIOSFODNN7` — a key name with the
credential's own value appended, which is how an exported link most often spells
one — end in the value, cleared every tier, and were published. An earlier round
found that and left it open, reasoning that closing it meant promoting `key` to a
plain component match and refusing `key_metrics` and `key_takeaways`. That was
the wrong shape for the fix rather than a reason to keep the hole: `key` alone is
ambiguous, `api key` and `access key` and `secret key` are not, on any page, in
any spelling. So the rule is about the PAIR — a `key` component with a qualifier
standing directly in front of it — and it needs no new judgement about ordinary
English. `key_metrics` has no qualifier before `key` and still passes; `sort_key`
is still caught by the suffix rule. The term the refusal renders is the two
declared literals joined, so it is still this file's vocabulary and not the
caller's bytes.

The FRAGMENT gets the same key screen as the query, via
`credentialFragmentError`. The query tier alone missed the single most likely way
a live token reaches this gate: the OAuth implicit flow returns its bearer token
AFTER the `#`, so a URL pasted out of a logged-in browser can carry
`#access_token=…` with no query string at all. A fragment written in `key=value`
form is screened key by key; an undecodable one fails closed for the reason the
query does.

A BARE fragment — no `=` anywhere — used to pass unconditionally, on the reasoning
that it has no key to test. That was sound only while the fragment was stripped
from the published destination: there was no publish path behind it. Publishing the
fragment made it one, and `#access_token` standing alone would have gone out in the
tweet unexamined. A bare fragment is now run through the same classifier, under the
name-vs-category split the query path already uses: with no `=`, the whole fragment
landed in the KEY position, so its text may BE the credential, and the refusal names
the category without echoing it. Section anchors are what this had to keep working,
and they do — `register`, `agenda-day-2`, `speakers`, `sessions`, `session-track`,
`schedule`, `sponsors`, `venue`, `keynote`, `day-pass` all clear the classifier;
measured, not assumed. Bare `#session` is the one realistic anchor it refuses, and
that is the exact-set entry earning its place elsewhere. The honest limit is that
this catches credential-NAMED shapes, not every credential: the classifier is a
denylist over names, so a raw `#eyJhbGciOi…` with no recognizable word in it still
passes.

The query is parsed with `url.ParseQuery` and the gate fails CLOSED on its error,
NOT with `u.Query()`, which discards that error and returns whatever pairs it
decoded. A query Go refuses to decode — an unescaped `;` separator, a bad escape
— therefore arrived as an empty map, and the screen cleared a URL whose
parameters it had never read.

What counts as a URL is decided twice, by two scanners with different jobs.
`tweetURLRe` finds the `http(s)://` runs X wraps in a t.co link, and it is the scanner
`weightedTweetLen` and `textCarriesURL` read as well. It carries no `\b`: Go's `\b` is defined over `\w`, which
includes `_`, so there was no boundary between the underscore and the `h` of
`_https://…` and the whole run went unseen. RE2 has no lookbehind, so `findTweetURLRuns`
applies the boundary in code instead — a run is at a boundary unless an ASCII letter or
digit precedes it. Only the LEADING delimiter is its business; a trailing `_` is swept into
the run like any other non-stop character, because `tweetURLTrailingPunct` does not list it.

ASCII is load-bearing there, and the wider `unicode.IsLetter` spelling it replaced was
wrong in the place this package already knows about. CJK copy puts no space before a link —
which is exactly why both stop sets list `。`, `、`, `！`, `？`, `，`, `：`, `；` — and the
wide boundary then read the CJK word in front as proof the link was not one, so
`登録events.example/r?access_token=…` was dropped unscreened and published. A run cannot be
"part of a longer word" when the script in front of it does not build words out of spaces.

**The boundary rule belongs to LINKIFICATION, and the credential screen does not use it.**
That was the second half of the same lesson, learned a round later. `findTweetURLRuns`
answers "what will X wrap in a t.co link" — right for `weightedTweetLen` and
`textCarriesURL`. The screen asks a different question: "what bytes are we about to
PUBLISH". An unlinkified credential is published just the same, so the screen reads
`findScreenURLRuns`, the same `tweetURLRe` matches with no boundary applied. Both helpers
read that one regexp so the difference between them stays exactly the boundary rule,
visible in one place.

The two sets diverge on one shape, and it is a shape real copy has: a missing space after a
word. `Register herehttps://host/r?access_token=…` is one keystroke from ordinary. It only
ever ESCAPED for a DOTLESS host, though — with a dotted one the scheme-less pass caught the
authority inside the run, because after `//` the position is bounded. That rescue was
incidental, not designed; `foohttps://intranet/x?api_key=…` offers
`schemelessScreenRunRe` no dot to match on and went out unscreened. It is the
`https://sup3r-s3cret/` shape the knowledge base names outright — a well-formed absolute
URL whose whole content is the token, sitting in the host.

`schemefulRunMask` masks that same unbounded set, and the two must be kept equal. Mask less
than the screen covers and the scheme-less pass re-reports an authority already checked;
mask more and a run nothing screened is hidden from the pass that would have caught it.
Widening the screen is safe in the one direction that matters: it is a strict widening of
what gets CHECKED, so the only new outcome it can produce is a refusal, never a
publication — the same trade `urlRunStartIsBounded` already accepts for `caféhttps://…`.

The second scanner exists because a link does not need a scheme to be published. X
linkifies `www.events.example/r?access_token=…` and bare `events.example/r?…` exactly as
it linkifies an `https://` one, and `tweetURLRe` requires a scheme, so an operator who
pasted a scheme-less link out of a logged-in browser had no guard at all.
`findSchemelessScreenRuns` covers that shape, and it feeds the SCREEN ONLY — the
weighting keeps its scheme-ful scanner, because matching X's t.co rules for scheme-less
links means implementing twitter-text's TLD grammar, and guessing at it near the 280
boundary rejects copy X would have accepted. Screening more than X links costs a refusal;
weighting more than X links costs a working brief.

What keeps that second scanner out of ordinary prose is that it requires a `?` or `#`. The
screen only ever asks whether a query or fragment parameter names a credential, so a run
with neither has nothing to read — which means `agenda.md`, `Node.js`, `v1.2` and every
other dotted token in real copy is never a candidate, and requiring the final label to
START with a letter keeps `3.2?` out too. Scheme-ful runs are blanked before the scan, by
byte offset, so a link is never screened twice.

The host forms that label accepts were widened once, and the first shape was wrong for a
reason worth keeping written down. It was `[a-z]{2,}` — which reads as "a TLD is a word",
and a TLD is not. A dotted-quad host is not, and neither is any internationalized TLD:
`xn--` is how every one of them is spelled on the wire, so that pattern was not missing an
exotic case, it was missing the entire non-Latin web. `198.51.100.7/r?access_token=…` and
`events.xn--p1ai/r?access_token=…` both cleared the screen and would have been published.
A dotted-quad alternative and a `[a-z][a-z0-9-]+` label cover both. Scheme-less bracketed
IPv6 is deliberately still not covered: X does not linkify it, and a leading `[` collides
with the markdown-link shape operators actually paste.

A scheme-less link with a PATH and no query or fragment — `events.example/reset/SECRET` —
is also not covered here, and that one is a DIVERGENCE from the snapshot redactor rather
than a gap in both. `internal/dispatch`'s `schemelessPathSnapshotRunRe` does match it. The
two sides are kept in step on what a link LOOKS like and deliberately not on what to do
about one, because their cost directions are opposite: that pattern's only discriminator is
a slash, and over-matching a slash on the redactor's side loses a fragment of the
operator's own copy from a diagnostic snapshot, while over-matching it HERE refuses a brief
X would have published, which no retry fixes. This screen also has nothing to read in such
a run — it asks whether a query or fragment parameter names a credential, and a path-only
run has neither — so mirroring the pattern would buy refusals and no new detection.

A THIRD pattern, `schemelessUserinfoRunRe`, covers the scheme-less shape that carries a
credential with no query to carry it: `user:password@host.tld`. The query scanner requires
a `?` or `#` because a query is the only thing it reads, so `bob:pw@events.example` was
screened by nothing while the scheme-ful `https://bob:pw@events.example` beside it was
refused. The bytes are published either way; whether X renders the run as a link does not
change what goes out in the tweet. The COLON is the entire discriminator and cannot be
dropped — without it the pattern matches `bob@events.example`, an ordinary email address,
and a screen that refuses those is worse than the hole it closes. The username before the
colon is RFC 3986's userinfo alphabet, sub-delims `!$&'()*+,;=` included, kept in step with
`pkg/redact`'s snapshot pattern: a narrower class let `admin!:pw@events.example` through both.
Its first character must be unreserved, so `Keynote (14:00@main.stage)` starts at the digit;
a username made only of sub-delims (`!:pw@host`), or a blank one (`:pw@host`), is a second
alternative held to a colon right after it. The clock exemption is `redact.UsernameIsClock` — one copy, shared with the snapshot
redactor — and is exactly: an all-digit pair of at most two digits a side (`14:00`, `3:4`), so
`2024:1234@ops.example` is refused, OR any sub-delim except `+` (`Mon,9:30`, `Session;9:30`) followed by a real clock
— hour 1–2 digits ≤ 23, password exactly two digits ≤ 59 (`Mon,9:30@main.stage`). A `+` prefix
never qualifies, so `alice+9:30@ops.example` and `alice+2024:1234@ops.example` are refused.

The colon is not QUITE the whole discriminator, and the round that shipped believing it
was put a false REFUSAL into the pre-create path. `keynote 14:00@events.example` and
`session 9:30@main.stage` are the RFC 3986 userinfo production byte for byte, and an
events platform writes that sentence every day. `userinfoRunIsClockShaped` skips a run
only when `redact.UsernameIsClock` calls its pair a clock (the exact rule is in the paragraph
above): an all-digit pair of at most two digits a side — a clock, a score, a ratio — or a real
clock behind any sub-delim except `+`. The password must be digits too, because the
narrower "numeric username" spelling gives up `9:hunter2@events.example` for nothing; a long
numeric pair (`2024:1234@…`) is a user ID and PIN and is refused. The negative rows that
missed this all happened to put punctuation between the clock and the host; hard against the
host is the shape real copy has.

U+FE0E is the one variation selector `emojiClusterLen` must NOT absorb. It requests TEXT
presentation — it is the codepoint that says "do not render the one before me as an
emoji" — so no RGI emoji sequence contains it and twitter-text's generated data has none.
Ending the cluster before it is therefore what twitter-text does, not a guess about it:
`U+1F5A5 U+FE0E` weighs 4 there, and absorbing the selector charged 2. The fix needs none
of the generated sequence table that the broader clustering question still turns on.

The error never renders the caller's key. It points at the offending parameter by
the fixed VOCABULARY WORD that classified it — `credentialQueryKeyMatch` returns
the matched literal alongside the verdict — and that word is always an entry from
this file's own lists, never a slice of caller text.

Two earlier rounds got this wrong in the same direction, and the reasoning that
failed is worth keeping: the secret is the VALUE, so naming the KEY is safe. It is
not. A parameter NAME is free text too, and `?oauth_token_<secret>=x` classifies on
`oauth` and then reproduced the secret in an error that reaches the dispatcher, the
campaign's persisted `Steps` and the service log. Truncating to 40 runes bounded
that without redacting it, and a credential prefix is still credential material.
The knowledge base states the test this fails — reproduce a component only when it
is BOTH structurally incapable of holding a secret AND load-bearing — and a
caller-written name fails the first half. A word we wrote passes it by construction,
and stays load-bearing, because the operator finds the parameter by searching their
own URL for that word, which is exactly how they would have used the key. It is the
same default-deny shape as `safeCause` in `internal/platform/hubspot/client.go`.

`queryKeysWrittenWithAValue` still splits name from category, because the two are
different refusals: it re-reads the raw query — `url.ParseQuery` cannot answer this,
giving the empty string for the value of both `?token=` and `?token` — and a bare
component, whose whole text landed in the key position, names no word at all and
redacts the URL instead. Safety is tracked PER OCCURRENCE, not per
key: one `=` anywhere used to be enough, so
`?oauth_token_SECRET&oauth_token_SECRET=x` decoded to a single key whose valued
occurrence marked it renderable, and the error then reproduced a string whose BARE
occurrence is the whole credential. A duplicated key is a strange thing for a brief
to carry, which is the point — the one shape that defeats the check is the one
nobody writes by accident, and requiring every occurrence to be named costs nothing
on ordinary input. The same split governs the fragment: the round that added
`credentialFragmentError` rendered its key unconditionally, so
`#access_token_<token>&state=…` — where the credential IS the component text —
was reproduced in the refusal. The check runs in the up-front block so the refusal
costs a corrected brief rather than an orphaned campaign.

USERINFO is refused outright, before the query is read at all.
`validateRegistrationURL` already rejects `https://user:password@host/…`, but a
link the caller pasted into their own copy never passes through that validator,
and this gate read only query keys — so an embedded credential in a URL with no
query string at all was published verbatim. Neither half of the userinfo is named
in the refusal; the URL is redacted, which is what locates the offending link
without the error becoming the leak it exists to prevent.

"Redacted" here means `redactURLForError`, and it keeps SCHEME AND HOST ONLY. It
kept the path too while its only caller was an operator-typed registration URL;
that stopped being defensible once the same helper began screening arbitrary
caller copy, because a magic-link or reset credential sits in a path segment
(`https://example.com/reset/<secret>`) at least as often as in a query parameter.
The knowledge-base rule is the test: reproduce a component only when it is BOTH
structurally incapable of holding a secret AND load-bearing for the diagnosis. A
path is capable; a host is not, and the host is what tells the operator which link
to fix. The stronger form applies to every caller in the package rather than only
the newer branches — dropping more can only cost specificity, never leak, and a
redactor whose strength depends on which caller reached it is one nobody can
reason about. The googleads client has a mirror of this helper that still keeps
the path; the same change is worth making there, and is not made here only because
that client's URLs do not flow into published text.

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

"Verbatim" is now literally true, and was not. The builder parsed the query and
re-emitted it with `url.Values.Encode`, which SORTS keys and re-canonicalizes
escaping — `%20` becomes `+` — so the promise this file and `docs/api-catalog.md`
both make was broken by the very step that claimed to keep it, on the one URL in
the flow where byte fidelity is the whole point. `url.ParseQuery` is still called,
but only to VALIDATE; `appendUTMToRawQuery` then copies each pre-existing component
as BYTES and appends the UTM pairs in sorted key order. Only a component whose
decoded name COLLIDES with a UTM key is dropped, because a destination carrying two
`utm_source` values makes click attribution depend on which one the landing page
reads first.

REASSEMBLY was the last gap in that promise. Splitting on `&` and rejoining
normalises a query's empty components, so `a=1&&b=2&` came back as `a=1&b=2` —
equivalent to every parser that will read it, and still a rewrite of a destination
that needed no rewriting, performed on the path where nothing collided at all. When
no pre-existing name decodes to a UTM key the original `rawQuery` is now used as
written, with the UTM suffix appended after it; the split runs only on the collision
path, and preserves empty components there too, since an empty component cannot name
a UTM key.

The fragment is dropped in the DISPLAY form and published verbatim in the real
one. It used to be dropped in both, on the reasoning that it never reaches a server.
That is true of the server and false of the page: `#register` scrolls to and focuses
the registration form, and a hash-router SPA reads the fragment as the ROUTE, so
`https://events.example/#/register` stripped of its fragment lands on the front page
instead. Paid clicks went somewhere the brief did not ask for, and silently — the
create succeeded and every step the client prints showed a destination that looked
correct.

Stripping it also cost the screen its subject. `credentialFragmentError` exists to
refuse a credential-shaped fragment, but `buildTwitterUTMURL` removed the fragment
before the composed text reached `rejectCredentialQueryParamsInText`, so that arm
never once examined the registration URL it was written for — only links the operator
typed into their own copy. Publishing the fragment puts it back under the screen,
which is what protects it; the strip only hid it.

`composeTweetText` appends the destination only when the text does not already
carry it, and "already carries it" is decided by `textCarriesURL`, which extracts
URL runs with the same `tweetURLRe` + `trimTweetURLPunct` pair `weightedTweetLen`
counts with and requires a whole-run match. A `strings.Contains` test answered a
different question: a URL is a substring of any URL that carries it in a redirect or
tracking parameter, so copy holding `https://click.example.net/r?next=<dest>` read
as already having the destination, the append was skipped, and X wrapped the whole
run as the OTHER link — leaving the ad with no direct click destination at all,
silently, on a create that succeeded.

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

`authoredTweetStatus` names the id AND what to do with it, and the second half is
not decoration. Authoring is unconditional — Step 4 says so in its own comment, and
the guard is deferred to the LFXV2-2665 idempotency work — so the retry the operator
chooses decides between two very different outcomes: passing this id back as
`tweetId` skips authoring entirely and promotes the tweet that is already live,
while retrying without it publishes a SECOND tweet under the LF handle. The message
says both. Returning the id and stopping there left the operator to infer the one
thing that matters, which is how "the id is returned" gets read — as it was, in a PR
description — as "the retry reuses it". It does not; the message now says which
retry does.

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

## Line-item bid write (`bid_update.go`, LFXV2-2665)

Backs `TwitterDispatcher.WriteBid`. Source: the X Ads API v12 Campaign Management reference,
<https://docs.x.com/x-ads-api/campaign-management/reference> (line items: `POST` and
`PUT accounts/:account_id/line_items/:line_item_id`), and the guide
<https://docs.x.com/x-ads-api/campaign-management>, consulted 2026-10-05: `bid_strategy` is `AUTO`,
`MAX` or `TARGET`; `bid_amount_local_micro` is in micro-units of the funding instrument's
currency; `pay_by` includes `LINK_CLICK` and `IMPRESSION` (the LINK_CLICKS goal supports both,
IMPRESSION by default); `WEBSITE_CLICKS` prices as CPLC. The reference page is too long to fetch
whole from the authoring environment, so the enum spellings were confirmed against its search
index and example line-item response rather than quoted in full; the guards fail closed on
anything unrecognized.

**Decision**: `bid_amount_local_micro` is a max CPC only when `bid_strategy == MAX` AND
`pay_by == LINK_CLICK` (`LineItemBid.ManualCPC`). **The create path sends `bid_strategy: AUTO`**
(objective `WEBSITE_CLICKS`, no bid, no `pay_by`), so every X campaign this service creates is
refused (409) until an operator moves the line item to a manual max bid charged per link click.

- `BidMicros(amount)` — positive, finite, at most 1,000,000, rounded like `toMicroCurrency`,
  refused if it rounds to zero; refusals are `ErrBidAmountInvalid` (`BidAmountReason`).
- `GetLineItemBid(ctx, lineItemID)` — `GET line_items/{id}?with_deleted=true`; a pure read; 404 →
  `(nil, nil)`; reports `deleted`; an answer for another id is an error; an id failing the path
  guard is `ErrInvalidLineItemID`, and a connection account id failing it (empty, outside
  `accountIDRe`, or longer than `maxAccountIDLen`) is `ErrInvalidAccountID` — the sentinel
  `campaignBudgetPath` uses — both before any request.
- `UpdateLineItemBid(ctx, lineItemID, micros)` — takes a write-pacer slot, then `PUT` with ONLY
  `bid_amount_local_micro` in the OAuth-signed query string (never `bid_strategy`/`pay_by`).
  `idempotent=false`, so a 429 is NOT retried in-call and comes back UNCONFIRMED — no refusal
  from a retry can be reported as "nothing changed". Transport/3xx/5xx and a 2xx echo of another
  line item or amount are UNCONFIRMED; a definite 400 is an amount refusal (`bidAmountError`,
  this package's own sentence) ONLY when an error is `INVALID_PARAMETER` with `"parameter":
  "bid_amount_local_micro"` — the shape the X Ads error reference
  (<https://docs.x.com/x-ads-api/fundamentals/error-codes-and-responses>) documents. That
  reference lists no bid-specific code, so there is no code allow-list; a code substring match
  would also catch `FORBIDDEN` or a bid-unit mismatch. `apiError` now carries an unexported
  `errorParams` — the envelope's (code, parameter) pairs, bounded like `ErrorCodes` and never
  rendered by `Error()`. The budget write
  (`UpdateCampaignBudget`) takes the other route — it retries the 429 and marks a later definite
  failure `retriedUnconfirmedError`; the bid write does not retry, so it needs neither.

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

## Campaign budget read + write (`budget.go`, LFXV2-2665)

The platform half of `TwitterDispatcher.WriteBudget` (see
[internal/dispatch](internal-dispatch.md#x--the-campaigns-daily-_local_micro-only-under-a-reported-campaign-budget-optimization)).

- `GetCampaignBudget(ctx, campaignID)` — `GET accounts/:account_id/campaigns/:campaign_id`
  ([reference](https://docs.x.com/x-ads-api/campaign-management/reference), "Campaigns"). A pure
  read returning `budget_optimization` and the two amounts as integer micro-units, each with an
  "unparseable" flag (string, fraction, negative, overflow — never read as "not set"). A 404 or
  `deleted: true` is `(nil, nil)`; an answer about another campaign id is an error. X's campaign
  object carries no `account_id`, so the account-scoped path is the account check.
- `UpdateCampaignBudget(ctx, campaignID, micros)` — `PUT` of exactly
  `daily_budget_amount_local_micro` (the total is read, never written), in the query string and
  OAuth-signed like every v12 write, after a slot on the shared write pacer. Idempotent, so a 429
  is retried; the retry loop now counts retries (`doRequestAbsCounted`, a caller-owned counter —
  never state on the shared client), and a definite failure AFTER a retried 429 is returned as
  `retriedUnconfirmedError` (Unconfirmed), mirroring the Microsoft client's PR #255 fix — a
  pre-send dial failure on the retry included, since it proves only that the RETRY never left. The 2xx
  echo is checked: another campaign id, another amount, or a PRESENT `null` amount (X reporting no
  daily budget right after one was written) is an UNCONFIRMED `transportError`; an echo that
  omits the field is accepted, the 2xx being the confirmation.
- `BudgetMicros` shares the create path's bound (`maxBudgetUsd`) and rounding
  (`toMicroCurrency`); its refusals wrap `ErrBudgetAmountInvalid` with a client-safe sentence
  (`BudgetAmountReason`). X publishes no per-currency minimum or maximum for these fields — only
  that the daily amount should not exceed the total.
- Ids are validated before any request: the connection's account id with `accountIDRe` and
  `maxAccountIDLen` (`ErrInvalidAccountID`), the row's campaign id with `campaignIDRe`
  (`ErrInvalidCampaignID`).

**Budget model.** `BudgetOptimizationCampaign` (`CAMPAIGN`) is the shape `CreateCampaign` is
INFERRED to produce — inferred from its sending no `budget_optimization` and putting the daily
amount on the campaign, not observed on a live account. X's v11
announcement makes `CAMPAIGN` the default and the two models exclusive (under `LINE_ITEM` the
daily budget must be on the line item and absent from the campaign); the current reference page
instead lists `LINE_ITEM` as the only value and default. The dispatcher therefore writes only on a
reported `CAMPAIGN` and refuses a campaign reporting `LINE_ITEM` or omitting the field (409)
before any write. `CreateCampaign` deliberately keeps omitting `budget_optimization` rather than sending
`CAMPAIGN`: the current reference lists `LINE_ITEM` as the only POST value, so an explicit
`CAMPAIGN` is undocumented and could fail every create. `TestCreateSendsQueryParams` pins the
omission; the settings readback likewise compares the budget only on a REPORTED `CAMPAIGN`.

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
no embedded userinfo) up front. It maps `tweetText` straight into `CampaignInput`;
`tweetId` is TRIMMED there first, once, so the adapter's authoring gate and the
client's own emptiness test cannot disagree about it; and `AsUserID` is not the
caller's value but the one `authorizedTwitterAsUserID` returns — the connection's
declared identity wins, a caller value must match it, a caller sending none
inherits it, and the check runs only on the authoring path. See "Authoring a
promoted tweet" above
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

## Account monitor (`monitor.go`, LFXV2-2665)

Stateless primitives for X's REPORT-BACKED account monitor, driven across requests by
`service.Orchestrator.ReadReportedAccountCampaigns` exactly as Microsoft's are. Report-backed
for every `days` value, because X's synchronous stats endpoint is capped at 7 days per request
and has one 250-requests-per-15-minutes budget shared by every foundation on the LF token,
while a stats job covers up to 90 days (https://docs.x.com/x-ads-api/analytics).

- **`ListAccountCampaigns`** — `AccountTimezone` (GET the account root, `timezone`, loaded as
  an IANA zone; absent/unknown fails closed; a success is cached on the client for
  `accountTimezoneCacheFor`, one minute, so list-then-submit in one read costs one account GET,
  and failures are never cached), then `campaigns?with_deleted=false&with_draft=false&count=1000`
  and `line_items?campaign_ids=<≤200>&with_deleted=false&with_draft=false&count=1000`, both
  through `walkPages`: the STRICT cursor rule `ListAdAccounts` uses (only X's documented null
  `next_cursor` ends a walk; absent/empty cursor, a repeated cursor, absent/null `data` or the
  page cap are errors). Status is `entity_status` verbatim. Budgets are
  `daily_budget_amount_local_micro` / `total_budget_amount_local_micro` ÷ 1e6, account
  currency; null/absent is "no budget", anything but a non-negative JSON integer sets
  `BudgetUnparseable`. The flight comes from the line items (v12 campaigns carry none):
  the envelope `StartDate`/`EndDate` — earliest `start_time`, latest `end_time`, `EndDate`
  empty if any line item is open-ended — and `Flights`, the union of the line items
  (`flightRanges`: each line item's first through last local day, sorted, overlapping or
  touching ranges merged, disjoint ones kept apart, an open-ended one absorbing everything
  after it), so the gap between line items Sep 1–5 and Oct 1–5 is not reported as scheduled.
  Dates are in the ACCOUNT's timezone, the last day being the last day served (an end at
  local midnight belongs to the previous day). An unreadable time sets `FlightUnparseable`,
  and so does a line item whose `end_time` is not after its `start_time` (inverted or
  zero-length): it is rejected before the envelope or the union changes, never recorded as a
  scheduled day and never silently dropped.
- **`SubmitAccountCampaignReport`** — window `[start of today-(days-1), start of the day after
  today)` in the account's zone (`accountReportWindow`), sent as UTC instants, and always
  EXACTLY the days returned as the report's first/last day. A day's start is its local
  midnight, or — when a DST spring-forward skips 00:00 (America/Santiago on 2026-09-06,
  America/Asuncion …) — the first instant that exists on that day (`localDayStart`): plain
  `time.Date` normalizes the nonexistent midnight BACK to 23:00 of the previous day, which put
  an hour of the previous day into the window and dated the saved first/last day one day early.
  A day start that is not a whole UTC hour (X takes whole hours only) is refused with
  `ErrReportWindowNotWholeHours`, never floored — before any stats request or job is created,
  though the account timezone has been read by then (an account GET when the cache is cold); a
  90-day window over a DST fall-back (90 days and an hour) drops its earliest local day instead
  of trimming an hour, so it covers 89 whole days and says so — the account monitor response
  exposes those days as `metrics_window_start` / `metrics_window_end` beside the requested
  `days`. GET `stats/accounts/:id/active_entities?entity=CAMPAIGN`, then one POST
  `stats/jobs/accounts/:id` per ≤20 active campaigns (`entity=CAMPAIGN`, `granularity=TOTAL`,
  `placement=ALL_ON_TWITTER`, `metric_groups=ENGAGEMENT,BILLING`), each through the write
  pacer and never retried on a 429. Before the first POST, `reserveStatsJobSlots` reserves the
  whole batch's consecutive pacer slots atomically under `writeMu` (so no concurrent writer can
  interleave and push the batch past its deadline; the context is re-checked under the lock, so a
  caller cancelled while queued reserves nothing), refusing — nothing reserved, no job created —
  with `ErrStatsJobBudget` when the last slot (after any backlog already queued) plus
  `statsJobSubmitMargin` (2s) does not fit the context deadline's remaining time (wall-clock
  `time.Until`); each POST then waits for its own slot, so a deadline cannot fire mid-loop and strand created jobs in X's concurrent-job slots
  (the dispatcher wraps it as `domain.ErrAccountReportBudgetTooShort`). Returns ONE composite id — the jobs' `id_str`s
  comma-joined — or `NoActiveCampaignsReportID` (`"none"`) when nothing was active, which
  Check answers as a finished empty report without a request. More than
  `MaxMonitorActiveCampaigns` (`maxStatsJobsPerReport` 10 jobs × 20 = 200) active campaigns
  is refused with `ErrTooManyActiveCampaigns` before any job is created, not truncated. The
  dispatcher wraps the two permanent refusals as `domain.ErrAccountTooManyActiveCampaigns` /
  `domain.ErrAccountTimezoneUnsupported`, which the monitor endpoint answers with 409.
- **`CheckAccountCampaignReport`** — ONE GET `stats/jobs/accounts/:id?job_ids=<all>`. Any
  `FAILED`/`FAILURE`/`CANCELLED` job, or a `SUCCESS` with no `url`, fails the report; any
  `QUEUED`/`PROCESSING` job, or one missing from X's answer, leaves it pending; an unknown
  status is an error. When all succeeded, each file is downloaded WITHOUT OAuth signing (X:
  "requires no authentication"), only from an https URL whose host is exactly
  `statsFileHost` (`ton.twimg.com`, the host of X's documented job-result example — not a
  `*.twimg.com` suffix) or the client's own API origin; any other host, userinfo, or a non-https
  foreign URL is refused before a request. Capped at 8 MiB compressed / 32 MiB decompressed
  (client fields defaulted from `defaultStatsFileCompressedCap` / `defaultStatsFileDecompressedCap`;
  the unexported `withStatsFileHosts` / `withStatsFileCaps` options are test seams that let a TLS
  httptest server stand in for the file host and lower the caps), gunzipped when it carries the gzip magic
  number, decoded with the synchronous stats types, and folded per campaign (impressions,
  clicks, `billed_charge_local_micro`); a URL never appears in an error. `Partial` is always
  true: X's billed charge is an estimate for days afterwards.
- **`ValidateMonitorAccountID`** — the connection's own `^[A-Za-z0-9]+$` + 64-character rule,
  untrimmed, mirrored by the design `Pattern`/`MaxLength` and pinned by the drift test.
- **`WithClock`** — new option setting the client's clock, which the window, OAuth timestamp
  and pacer all read.
- `time/tzdata` is embedded so `time.LoadLocation` works on the static base image.

The whole file is UNVERIFIED CONTRACT against a live X account; each relied-on claim cites
docs.x.com next to the code.

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

## Line-item keyword targeting (`keyword_targeting.go`, LFXV2-2665)

`ListLineItemTargetingCriteria` lists a line item's targeting criteria
(`targeting_criteria?line_item_ids=…&with_deleted=false&count=1000`, cursor-walked through
`cursorVerdict` like `findByName`) and is all-or-error: no result set, a criterion without a
usable id or under another line item, an unusable cursor on a full page, or the page cap is
`ErrTargetingUnreadable`. `DeleteTargetingCriterion` takes a write-pacer slot (a wait cut short is
`ErrWriteNotSent`, and so is a request `ProbeNotSent` proves never left the process — a DNS or
connect-time failure, or a context already done at entry — checked before any API
classification, so the dispatcher reports `NOT_SENT` rather than `REJECTED`), sends one DELETE with the 429 never retried, maps 404 to
`ErrTargetingCriterionNotFound`, and accepts a 2xx only when it names the criterion with
`deleted: true` — anything else is an UNCONFIRMED `transportError`. The create path sets no
targeting criteria. See [Keyword Targeting on Reddit and X](../architecture/keyword-targeting-reddit-x.md).

## Campaign-ref id rule (`campaign_ref.go`, LFXV2-2665)

`ValidateCampaignID` is the id rule `resolve-twitter-ads-campaign` applies before any lookup:
the package's `campaignIDRe` (`^[A-Za-z0-9]+$`) and at most 64 characters, untrimmed. Returns
`ErrInvalidCampaignID`. It contacts nothing.

## Adoption read (`campaign_lookup.go`, LFXV2-2665)

`GetCampaign` is the read `TwitterDispatcher.LookupCampaign` makes to adopt an existing campaign:
one `GET accounts/:account_id/campaigns/:campaign_id` — the resource the budget read and the toggle
address — through `request()`, so a 429 is retried (and a declared reset past the wait cap ends the
read at once) and an exhausted one is an error. `ValidateCampaignID` (alphanumeric, ≤64, no
padding) runs first and returns `ErrInvalidCampaignID` with no request. `deleted: true` is `(nil, nil)`,
and so is a 404 — but only once ONE confirming read of the connection's own account
(`GET accounts/:account_id`, `confirmAccountReadable`) answers 2xx naming that account; any other
answer to it (404, 401/403, 5xx, throttle, a body naming another account) is an error. The
confirming read is made only after a campaign 404; `entity_status` `ACTIVE`, `PAUSED` or `DRAFT` is a ref (a draft campaign exists);
any other status, a missing name, an id echo that differs, an empty `data`, a 401/403, and a body
`identityjson.Check` refuses or that does not decode are errors. The read is path-scoped; X's
campaign object is not documented to carry `account_id`, and when a response does carry one it is
returned for the dispatcher to compare.

## Settings readback read (LFXV2-2665)

`GetCampaignSettings(ctx, campaignID, lineItemID)` (`campaign_settings.go`) reads the campaign
(name, `entity_status`, `budget_optimization`, the daily and total `*_local_micro` amounts,
`account_id`) and, when the row recorded one, the line item (`start_time`, `end_time`,
`bid_strategy`, `campaign_id`; `with_deleted=true`). Both bodies pass `identityjson.Check` over
the RAW body and a strict id echo. A 404 or deleted campaign is `(nil, nil)`; a 404 or deleted
line item only leaves `LineItem` nil; a line item of another campaign is
`ErrLineItemNotInCampaign`; an amount that is not a non-negative integer is an error.

## Audience insights read (`audience.go`, LFXV2-2665)

`GetAudienceInsights(ctx, window, campaignIDs)` reads AGE, GENDER and PLATFORMS segmentations
over the project's own campaigns. X serves segmentation only through the ASYNCHRONOUS stats-jobs
API (the synchronous `stats/accounts/:account_id` takes no `segmentation_type`), so one call:
`GET accounts/:account_id` (timezone required; `currency` optional, ISO 4217 when present; id
must echo the account; `identityjson.Check` on the raw body) → `reserveStatsJobSlots` → one slotted
`POST stats/jobs/accounts/:account_id` per segmentation per batch of ≤20 ids (`postStatsJob`,
shared with the monitor's `createStatsJob`: `entity=CAMPAIGN`, `granularity=TOTAL`,
`placement=ALL_ON_TWITTER`, `metric_groups=ENGAGEMENT,BILLING`, plus `segmentation_type`) → one
`GET stats/jobs/…?job_ids=<all>` per poll (first immediate, then every
`audiencePollInterval`, at most `audienceMaxPolls`) → each results file through
`downloadStatsFile`. The read WAITS inside the caller's deadline instead of persisting a report
id: unfinished jobs are `ErrAudienceJobsUnfinished` (503) and are left to expire on X.
Scope: non-empty, every id `ValidateCampaignID` (`ErrAudienceScopeInvalid`), de-duplicated, at
most `MaxAudienceCampaigns` (40 — a local bound: two batches, six jobs; `ErrAudienceScopeTooLarge`).
Window (`AudienceWindowBounds`, exported so the dispatcher's cache can tell whether a result's
window still names the same instants): the metrics read's window NAMES (today; yesterday; today
and the six before) but on the ACCOUNT's calendar via `localDayStart`, `[start, end)` — the
metrics read (`dateRangeForWindow`) uses UTC days, so on a non-UTC account the instants differ; non-whole-hour bounds are
`ErrReportWindowNotWholeHours`; other windows `ErrUnsupportedWindow`. Trust: `identityjson.Check`
on every job, status and file body; a status answer naming an unasked or repeated job, a
FAILED/CANCELLED job or a SUCCESS without url fails; every file entity must be in THAT job's
batch, once; `segment.segment_name` is required and must match `audienceValueRE` (returned
verbatim, never echoed); a repeated (campaign, segment) fails; counters are absent/null → 0 (X's
"no activity"), else exactly one non-negative integer bucket, summed with an int64 overflow
guard; CTR after summing. Ordered by dimension, impressions desc, value. Tests:
`audience_test.go` (stateful stub; segmentation params, batching and bound, account-tz windows
incl. DST and a UTC/local day split, polling bound and deadline, duplicate/case-folded keys at
every level, null counters, malformed files, 401/403/429/5xx, job defects, budget check).
`AllCountersNull` is set when a whole dimension (all batches) returned rows with no measured
counter (`audienceCounter` reports `measured`; a literal 0 counts): X's forum reports segmented
jobs that succeed with every metric null, and that is indistinguishable from an idle campaign set
(this package reads X's null as "no activity" — `statsFold`, `firstOrZero` — and X does not
document idle entities as omitted), so failing closed would 503 every idle project; the flag
keeps the zeros from passing as a measurement instead. Job POSTs use `reserveStatsJobSlots`
(shared with the monitor; see above), so a read never starts a batch it cannot finish in its
budget and no concurrent writer can split the batch. A failure that leaves jobs running on X —
any failure while creating or awaiting them — is `*AudienceJobsAbandonedError` carrying the job
ids X confirmed (Unwrap keeps the sentinels); `RunningStatsJobs(ctx, ids)` answers which of them
X still lists as not finished (one status read, `identityjson`-checked; an answer naming an
unrequested job or one job twice is an error, so a malformed answer never releases a job; queued,
processing, unlisted and unrecognised all count as running), and `AudienceJobCount(ids)` is how many jobs a
read would create — both for the dispatcher's per-account job budget. UNVERIFIED against a live account: the segmented file shape and
`segment_name` vocabulary.

## Account-keyed write pacer (`pacer.go`, LFXV2-2665)

`AccountPacers` hands out one `writePacer` (mutex + next-write instant) per X ad account (keyed by
base URL + account id); a client built `WithAccountPacers(reg)` paces and reserves stats-job
batches (`pace`, `reserveStatsJobSlots`) against its account's pacer, so two projects'
connections to the shared LF account — or a cache replacement — cannot interleave writes or
split a batch. A client without a registry, or without an account id (discovery), paces
privately. The dispatcher owns the registry, so the scope is the PROCESS; replicas do not share
it, which is why only the pod holding the per-account stats-job lease (`postgres.StatsJobLease`)
creates stats jobs, with the chart's replica refusal as a first line of defence.
`createAudienceJob` reports `maybeCreated` for a failed create that may have committed
(`createOutcomeAmbiguous`, or any unusable 2xx), and `GetAudienceInsights` counts those as
`AudienceJobsAbandonedError.Unknown` — also when no job was created before — so the dispatcher
can charge them. Tests: `TestAccountPacers_ClientsForOneAccountShareReservations`,
`TestGetAudienceInsights_AmbiguousCreatesAreCharged`.
