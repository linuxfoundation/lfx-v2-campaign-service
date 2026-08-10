---
type: "Go Package"
title: "internal/platform/hubspot"
description: "HubSpot API client (email channel): bearer auth, request layer with 429 retry, marketing-email + CRM-list + event-def operations, and marketing-email statistics reads."
resource: "internal/platform/hubspot"
tags:
  - platform-client
  - hubspot
  - email
  - go-package
timestamp: "2026-07-20T00:00:00Z"
---

# internal/platform/hubspot

Package hubspot is the HubSpot API client for the EMAIL channel (LFXV2-2770). It
drives HubSpot's email surface: marketing-email search/get/clone, draft-update
(subject + sender) and draft rich-text CONTENT read/write (GetEmailHTMLWidgets /
SetEmailHTMLWidgets, added with UTM link tagging in LFXV2-2775 — the write patches
only the named widgets' body.html so untouched template configuration survives), CRM
contact-list search/get/create/filter-update (no delete), and event-definition
lookups. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database. In production the bearer token is a field inside the
connection's ENCRYPTED credentials blob (there is no `private_app_token` column on
`hubspot_connections`); the connection layer decrypts it and injects it here.

## Auth (simplest of all the clients)

Unlike the ad-platform clients — Google Ads (OAuth2 refresh→access flow), X (OAuth
1.0a signing) — HubSpot authenticates with a **static private-app bearer token**.
There is no token-exchange endpoint: `doRequest` just attaches
`Authorization: Bearer <token>` from the injected `Credentials.PrivateAppToken`. A
missing token is a definite pre-send error.

## Request layer

`doRequest` applies the repo's standard discipline, mirroring the
googleads/reddit/meta/twitter clients:

- **No-follow redirects** enforced on whatever client is in use — including one
  supplied via `WithHTTPClient` — by rebuilding a FRESH `*http.Client` from the
  caller's reusable exported fields (Transport/Jar/Timeout) + `noFollow`, so the
  caller's client is never mutated. A 3xx is surfaced, not followed.
- **Bounded reads** (`maxResponseBody+1`, 10 MiB).
- **Typed body-free errors:** a non-2xx surfaces an `apiError` whose `Error()`
  renders only method/path/status — the response body is NOT retained at all (the
  request layer drains it for connection reuse but keeps no snapshot). Nothing in this
  client classifies on the body, and retaining it on an exported field could leak a
  HubSpot error envelope's request material via reflection/JSON of the error even though
  `Error()` omits it. A round-trip failure after the request was plausibly sent, or a 2xx
  whose body can't be read, is a `transportError`; it is ambiguous ONLY for a
  MUTATING call (`IsUnconfirmed` returns `transportError.Mutating`) — an idempotent
  read/search that failed in transit landed no mutation and is safely retryable. Its
  `Error()` peels
  every nested `*url.Error` layer (`safeCause`) so the request URL — which can carry
  query secrets — never leaks, while `Unwrap()` keeps the cause for `errors.Is/As`.
  A DNS/connect-time dial failure (`isPreSendDialError`) is a clean pre-send error
  (definitely not sent), rendered URL-free.
- **429 retry gated on an explicit `idempotent` flag:** a rate-limited idempotent
  call (a GET read) is retried up to `retryMax` with a bounded backoff honoring
  `Retry-After` (a server value over `maxRetryWait` aborts rather than sleeping
  pointlessly). A NON-idempotent call (a list/email create/clone) is NOT retried —
  HubSpot creates have no idempotency key, so a 429 whose first attempt may already
  have committed would double-create; it returns the 429 as an `apiError`
  immediately.

## Marketing-email operations (LFXV2-2779)

`email.go` builds on `doRequest`: `SearchEmails`/`GetEmail` (idempotent reads),
`CloneEmail` (`POST /marketing/v3/emails/clone`), `PatchEmailSettings`
(subject + the v3 `from` object's `fromName`/`replyTo`; preview/preheader text is
NOT a first-class v3 field, so it is deliberately not offered — see LFXV2-2775), and
`SetSendList`. Both `PatchEmailSettings` and `SetSendList` PATCH the DRAFT route
(`/marketing/v3/emails/{id}/draft`) — the base `/{id}` route mutates the LIVE email,
so draft edits must go through `/draft`. Creates/clones/PATCHes pass
`idempotent=false` (no idempotency key → a retried 429 could double-create). A clone
with a 2xx-no-id, and a clone/PATCH with an UNDECODABLE 2xx body, are surfaced as
UNCONFIRMED (a PATCH that returns a 2xx with no id just substitutes the requested id —
the update applied, so that is NOT unconfirmed). A mutating 429/3xx/5xx apiError is
flagged `Ambiguous` (see `IsUnconfirmed`), so the caller verifies rather than
blind-retrying. A GET (read) is never UNCONFIRMED — a malformed read is a plain error,
safely retryable.

**`SetSendList` recipients (ILS-only):** a HubSpot email's recipient list goes in
`contactIlsLists` (ILS list ids). HubSpot's ILS migration removed functional support
for the legacy `contactLists` recipient field after 2024-10-31 (it's silently
non-functional now), so this client NEVER emits `contactLists` — callers resolve an
ILS list id from the Lists v3 API. The client sends a COMPLETE `to` (clearing
`contactIds` so no clone-source contacts leak) with `contactIlsLists.include` = the
send list + `.exclude` = suppressions.

## CRM contact-list + event-definition operations (LFXV2-2780)

`lists.go`: `SearchLists` (`POST /crm/v3/lists/search` — constrains to contact lists
SERVER-SIDE via the `objectTypeId "0-1"` request field (a valid `ListSearchRequest`
field per HubSpot's v3 docs), with a per-hit `ObjectTypeID` check kept as
defense-in-depth; follows `offset`/`hasMore` pagination with a repeated-page guard;
`includeFilters` is a GET-single-list field, NOT a search field, and is not sent),
`GetList` (with `includeFilters=true`
so the filterBranch + processingType come back),
`CreateList` (`POST /crm/v3/lists` — canonical no-trailing-slash path, since the client
refuses redirects; DYNAMIC, contact objectTypeId `0-1`),
`UpdateListFilters` (`PUT …/update-list-filters`), and `ListEventDefinitions` (whose
human label is nested under `labels.singular`/`.plural`, not a top-level field, and
which does NOT request `includeProperties` — that payload is discarded). **List size
has TWO shapes** (`List.resolveSize` normalizes both): GET/CREATE
(`PublicObjectList`) carry a top-level integer `size`; SEARCH hits have no top-level
size and instead expose `hs_list_size` as a STRING under `additionalProperties`,
requested explicitly. `ListEventDefinitions` resolves `fullyQualifiedName` for
BEHAVIORAL_EVENT filters.

`filterBranch` is passed through as OPAQUE JSON — HubSpot's shape invariants (OR-root
with AND sub-branches, no nested ORs, `IN_LIST` not `LIST_MEMBERSHIP` in membership
branches) belong to the audience-builder (LFXV2-2774), not this transport client. A
create's 2xx-with-no-id is UNCONFIRMED. List/get responses are decoded from BOTH the
`{"list":{…}}` wrapper and the bare top-level shape HubSpot variously returns.

## Marketing-email statistics (LFXV2-3058)

`statistics.go` adds `GetEmailMetrics`, the read that backs the email channel's metrics:
`GET /marketing/v3/emails/statistics/list?startTimestamp&endTimestamp&emailIds`, an
idempotent read (so a 429 IS retried). It returns the same `model.CampaignMetrics`
every ad platform returns, plus an `Email` sub-object of six email-specific counters. Two
of the six deliberately overlap the shared fields: `Opens` also populates `Impressions`
(an open IS the recipient rendering the creative) and `Clicks` is the same event in both
channels, so a consumer reading the sub-object never has to know which ad-shaped field the
email channel was mapped onto. The other four — `Sent`, `Delivered`, `Bounces`,
`Unsubscribes` — have no ad-platform analogue.

The v3 contract was verified against HubSpot's own generated client rather than the
docs (which are auth-gated): the response is
`{"emails": [int], "campaignAggregations": {...}, "aggregate": {"counters": {...},
"ratios": {...}, "deviceBreakdown": {...}, "qualifierStats": {...}}}`.

**`counters` is an OPEN map** — the v3 schema types it `map[string]int` and enumerates
no keys. That is the whole reason for the guards below.

### The span selects emails by SEND date; it does not window the counters

This is the single most important thing to know before reading a number out of the
result, and it is easy to get backwards. The generated contract describes the operation
as returning "aggregated statistics of emails SENT in a specified time span", and
`emails` as the list of emails sent during it. So `startTimestamp`/`endTimestamp` choose
WHICH EMAILS are in scope, by send date. The counters that come back are that email's
totals to date — not the opens and clicks that occurred inside the span.

Two consequences, and neither may be papered over:

- A span containing the send date returns the email's aggregate counters. `today` and
  `last_30_days` on an email sent this morning return the SAME numbers.
- A span not containing the send date returns nothing at all, reported as
  `ErrNoSentEmailInWindow` (below) rather than as zeros.

`model.CampaignMetrics.Window` therefore records what was ASKED, not a period the
counters are scoped to. Presenting these as "opens in the last 7 days" would be false.
Genuine event-time windowing needs a different HubSpot source — the email-events API,
which timestamps each open and click — and is deliberately not attempted here.

### The five fail-closed guards, and why zeros were the wrong answer

A metrics read has a caller that acts on an absence: zeros read as "this campaign is
not performing", which is a decision-grade statement. So an answer this client cannot
VERIFY must be an error, never a clean zero.

- **`ErrUnrecognizedCounters`** — a `counters` map carrying not one key from HubSpot's
  counter vocabulary. Because the map is open, a renamed key set decodes cleanly to
  zeros: an email that really sent would report as having sent nothing. The probe set
  (`knownCounterVocabulary`) is deliberately WIDER than the six keys this client maps.
  An email that WAS sent in the window can legitimately return only `notsent`/`pending`
  — every recipient still pending, or suppressed per-recipient — with the six mapped
  counters zero and therefore omitted, and treating that as a vocabulary change would
  turn an ordinary all-zero result into an error. (Not the never-sent case: that one has
  an empty `emails` list and returns `ErrNoSentEmailInWindow` well above this guard, as
  the paragraph below says.) The guard distinguishes "the vocabulary is intact and
  these numbers are zero" from "the vocabulary changed and we are reading nothing" — so
  it must recognize the whole vocabulary while mapping only part of it.
  The guard is **not** conditioned on the map being non-empty. A renamed or dropped
  `counters` FIELD decodes to a nil map, which a `len(counters) > 0` test waves through
  — and that is the same schema break, arriving in the one shape the narrower check
  cannot see. Nothing licenses zeros here: an empty `emails` list returns
  `ErrNoSentEmailInWindow` before this guard is reached, so there is no path on which
  an all-zero counter map is a legitimate answer.
- **`ErrNoSentEmailInWindow`** — an EMPTY `emails` list. Per the contract above, the span
  selects by SEND time, so all this establishes is that no SENT email matched the id
  within it. It is deliberately not narrowed to "the send was outside the window": three
  states arrive in this identical shape — sent outside the span, a staged draft never
  sent, and an id that does not exist — and the response separates none of them. Naming
  it after the first would send a caller hunting for the right window for an email no
  window will ever find. What it is NOT is "the email earned no engagement": the email
  that really had no engagement comes back PRESENT, carrying a `sent` (or `notsent`)
  counter. Zeroing the empty case would make those two the same answer, which is the one
  where a live campaign reads as a dead one.
- **`ErrStatisticsFilterNotHonored`** — the response's `emails` list is non-empty and is
  not EXACTLY the id we filtered on. Omitting it is the obvious case; naming it
  alongside others is the same failure and the one a presence check admits. The request
  supplies a single `emailIds` value and `aggregate` is the aggregation over the emails
  the response covers, so a wider list means the aggregate carries strangers' sends —
  attributing them to this campaign is exactly what the guard exists to prevent. Either
  the filter was honoured, in which case the list is what we asked for, or it was not,
  in which case none of the response is trustworthy. An EMPTY list is a different
  failure and is handled by the guard above, not by this one. An ABSENT `emails` field
  is a third case and belongs here rather than with the empty one: the field is the only
  evidence the filter was applied, so its disappearance means the aggregate describes an
  unknown set. `emails` is therefore decoded as a POINTER — a value slice would decode
  both `null` and `[]` to nil and collapse "the schema broke" into "wrong window",
  sending the caller off to retry other windows against a shape that can never answer.
- **`ErrRenamedCounter`** — the PARTIAL rename the vocabulary guard cannot see. A map
  like `{"sent":1000,"emailsOpened":400}` keeps a recognized key, so
  `ErrUnrecognizedCounters` passes, while the `open` lookup returns an authoritative 0
  for an email with 400 opens. The guard fires only when a MAPPED key is absent AND an
  unrecognized key is present, and requiring both is the design rather than a
  convenience. A missing mapped key alone proves nothing — HubSpot may omit a
  zero-valued counter, and the spec that would settle it is auth-gated, so rejecting
  absence would fail ordinary quiet emails. An unknown key alone proves nothing either:
  ADDING a counter is the likeliest way this vocabulary evolves, and rejecting additive
  change would break the client on a release that removed nothing. Only the conjunction
  carries the signature of a rename, because a renamed key does not vanish — it
  reappears under another name. What still slips through is a rename with no new key
  visible in the same response; the widened `ErrUnrecognizedCounters` catches the whole-
  vocabulary version of that, and nothing catches a silent single-key drop.
  The two signals CAN co-occur innocently — an upstream release adds a counter in the
  same week an email omits a zero-valued one — and that case errors on purpose. With
  both present the client cannot distinguish addition-plus-omission from a rename, and
  the rule this section opens with applies: an answer it cannot verify is an error, not a
  clean zero. The asymmetry is what settles it — a false "cannot read this" is recovered
  by adding the new key to the probe set, while the false zero is a live campaign
  reported as dead and nobody goes looking. The quiet `notsent`/`pending` path is
  untouched by all of this: those keys are KNOWN, so no unknown key is present and the
  guard cannot fire however many mapped keys are absent.
- **`ErrNegativeCounter`** — any counter below zero. These are event counts, so a
  negative is malformed upstream data; passed through it becomes negative impressions
  and a negative CTR that reads as authoritative. Checked across the WHOLE map rather
  than the six mapped keys: a negative anywhere is evidence the payload is wrong, and
  the keys we read are no more trustworthy for having stayed positive. The LinkedIn,
  Meta and Reddit readers reject negatives for the same reason.

`campaignAggregations` is deliberately NOT decoded: it is keyed by email-CAMPAIGN id,
not by email id, so indexing it with the id we filtered on would silently miss and fall
through to a zero value. Since the request filters to exactly one email, `aggregate`
already IS that email's aggregate.

The `emailID` (the campaign's `PlatformCampaignID`) is validated as a CANONICAL
positive decimal integer — the round-trip compare rejects `+42`, `042` and padded forms
that `ParseInt` otherwise accepts. HubSpot types `emailIds` as an integer list, so a
value that is not one would either be rejected upstream or, worse, ignored — widening
the filter to the whole portal.

### Windows

`timeRangeForWindow` maps all seven `model.MetricsWindow` values; HubSpot takes
arbitrary ISO-8601 instants, so there is no platform vocabulary to translate. Windows
are anchored to the UTC calendar date, matching the LinkedIn adapter, even though
HubSpot's own UI reports in the PORTAL's timezone — a portal on America/Los_Angeles and
a `today` read here can legitimately disagree at the day boundary. UTC is chosen because
a cross-channel report needs one answer across every platform, and matching each
portal's zone is not achievable anyway (the ad platforms have their own). `end` is the
last millisecond of the final day rather than next-midnight: HubSpot does not document
whether `endTimestamp` is inclusive, and under either reading this range is wrong by at
most a millisecond, where next-midnight would gain a whole extra day if the bound is
inclusive. Month boundaries are computed from the first of the month, never
`AddDate(0, -1, 0)` on today's day-of-month — `AddDate` normalizes an invalid
day-of-month into the following month, which would shift `this_month`/`last_month` on
the 29th–31st.

`ValidateMetricsWindow` is exported so a caller can reject an unsupported window BEFORE
resolving credentials: an unsupported window is a permanent 400 whatever the
connection looks like, and resolving first would surface a connection problem instead
and invite the caller to retry a request that can never succeed.

### `cost_micros` is 0, and that is not "free"

HubSpot bills no per-send cost, so there is nothing to read and the field is 0. The
meaning is "this platform bills no per-send cost", NOT "this campaign was free" — email
spend lives in the subscription. A consumer blending this 0 into a cross-channel
cost-per-acquisition understates the real cost, and the field's shape gives no hint of it,
so it is stated wherever a consumer might meet the field: the model doc and here.

The Goa description at `design/brief.go` is **not** one of those places yet — it still
carries the generic per-platform cost wording, which says nothing about an email channel
that reports zero. Saying so there is part of the endpoint work in part 2, where the email
window reaches the API surface; recorded here so the gap is a known one rather than an
oversight.

## Authenticated portal resolution (LFXV2-3058)

`AuthenticatedPortalID` resolves the HubSpot hub (portal) id that the private-app
bearer token actually authenticates against, by POSTing the token as `tokenKey` to
`/oauth/v2/private-apps/get/access-token-info` and reading `hubId` from the reply. This is
deliberately NOT `AccountConfig.PortalID`, which is an optional operator-supplied string
used only to build app URLs; nothing keeps it in step with the token, so a credential
swap can leave the configured value pointing to one portal while the token reaches
another. Only the token's authenticated identity is authoritative, and this method is
how to query it.

The id is returned as a string because it is compared against stored campaign
`Result.PortalID` values (which are also strings) at metrics-read time. HubSpot email
IDs are bare numerics unique only within a portal, so an email id can collide across
portals — reading it under the wrong portal silently returns another portal's counters
or false "no data". Both are wrong, so the dispatcher records the authenticated portal
at dispatch time and refuses a metrics read when the token has moved to a different
portal.

**Why not `/account-info/v3/details`.** That endpoint returns the same id as
`portalId` and was the first implementation, but HubSpot documents it as requiring
the `oauth` scope, and `oauth` is not a scope a private app can hold — it does not
appear in the private-app scope picker, being the implicit scope of an
OAuth-installed public app. A private-app token is rejected there in every account,
not merely under-scoped ones. Because both callers treat a failed lookup as "portal
unknown", shipping it would have meant `Dispatch` never recording a portal and
`ReadMetrics` fail-closing on `ErrCampaignProvenanceUnknown` permanently: a guard
correct in every test, wired end to end, returning nothing in production. Caught in
review on PR #113 before it shipped.

The token travels in the request BODY rather than only the `Authorization` header.
That is safe here because this client's errors are typed (`preSendError`,
`transportError`, `apiError`) and render method and path only — no request body
reaches an error string. Re-check that property before putting any other secret in
a body.

This call is sent by both code paths: `Dispatch.cloneEmail` (best-effort, wrapped in
a short timeout and logged as a warning if it fails, so a provenance lookup cannot
block an otherwise-ready send) and `ReadMetrics` (fail-closed, so a token credential
problem is discovered at metrics time, not at send time). Best-effort does not mean
expected to fail: with the correct endpoint that warning should be rare, and a
steady stream of it is a real signal.

Malformed responses (non-JSON or missing/non-numeric `hubId`) do not leak upstream
data into logs; the error message is fixed text + response length.

## Scope

Auth + request layer + the email/list/event-def operations above, plus marketing-email
statistics reads and authenticated portal resolution. Consumers: the audience-building
logic (LFXV2-2774, uses lists + event-defs) and the email staging dispatcher
(LFXV2-2777, uses the marketing-email ops) and metrics reader, the latter blocked on
PR #11.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` hubspot adapter (see [internal/dispatch](internal-dispatch.md)) is
the EMAIL channel (not an ad platform), using a single private-app token. Unlike the ad
adapters (which CREATE a campaign) it STAGES a marketing email: it CLONES a
caller-specified template (`hubspotConfig.sourceEmailId`) and points the clone's send list
at the brief's BUILT audience — resolved from the `campaign_audiences` resource
(LFXV2-2773) via an injected `audienceReader`, taking the newest hubspot audience and
refusing if it is not yet `built` (`PlatformMasterListID` → the send list,
`SuppressionListIDs` → exclusions). The cloned email's HubSpot id is the campaign's
`PlatformCampaignID`; the clone is a DRAFT (a human sends it). AI body content
(LFXV2-2775) and audience building (LFXV2-2774) are separate steps. Claim contract: an
UNCONFIRMED clone (2xx-no-id / transport) retains the claim with a name-only partial; a
post-clone send-list failure is a partial (the email exists — retain + reconcile); a
definite pre-clone failure releases the claim.

It has no `StatusToggler` implementation — the email channel has no run state to pause or
resume.
