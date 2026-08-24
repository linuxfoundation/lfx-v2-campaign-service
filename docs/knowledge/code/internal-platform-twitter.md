---
type: "Go Package"
title: "internal/platform/twitter"
description: "X (Twitter) Ads v12 client: OAuth 1.0a signing, ad-account discovery, and the campaign -> line_item -> promoted_tweet creation flow."
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
Budget is likewise validated pre-create (positive, ≤ 1e9, rounds to ≥ 1 micro-unit). The client paces sequential writes within a SINGLE dispatch
toward the 1-req/sec limit; it does NOT enforce the account-wide write limit
across concurrent dispatches or replicas (that needs cross-replica coordination,
tracked in LFXV2-2665), so operators must not rely on this stateless client for
cross-dispatch rate limiting. When the account limit is hit anyway, 429s are
retried with backoff bounded by `Retry-After` / `X-Rate-Limit-Reset`. If the caller's
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
  a present `[]` yields a non-nil empty slice, while both absent and null leave it nil.
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
no embedded userinfo) up front. `validateTwitterConnection` holds the credential rules
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
