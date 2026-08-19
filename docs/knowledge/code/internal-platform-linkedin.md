---
type: "Go Package"
title: "internal/platform/linkedin"
description: "LinkedIn Marketing API client: OAuth2 dark-post campaigns (Campaign Group -> Campaign -> Dark Post -> Creative) with targeting, up-front validation, campaign status toggle, live analytics reads, ad-account discovery, and optional single-flight access-token refresh."
resource: "internal/platform/linkedin"
tags:
  - platform-client
  - linkedin
  - linkedin-ads
  - oauth2
  - go-package
  - metrics
  - account-discovery
  - token-refresh
timestamp: "2026-08-09T20:00:00Z"
---

# internal/platform/linkedin

Package linkedin provides a Go client for the LinkedIn Marketing API, ported
from the upstream TypeScript `linkedin-ads.service.ts` client. Credentials and
the full runtime config are injected via `NewClient`; the client never reads the
process environment or files.

Authentication is a Bearer access token; every request also sends the
`LinkedIn-Version` and `X-RestLi-Protocol-Version` headers required by the
Marketing API.

**Token refresh is OPTIONAL.** LinkedIn issues programmatic refresh tokens only
to approved Marketing Developer Platform partners, so `Credentials` may carry an
access token alone; `CanRefresh()` gates the whole path on `RefreshToken`,
`ClientID` and `ClientSecret` all being present, and a bearer-only connection
behaves exactly as it did before refresh existed.

**Every validator gates on the TRIMMED value, so padding is refused at the WRITE
boundary, never trimmed at the read.** `CanRefresh()` tests
`strings.TrimSpace(...) != ""`, so a stored `" client-id "` satisfies it and every
other presence check, while the store keeps the value verbatim — and the exchange
then fails at LinkedIn as `invalid_client` on every refresh, forever, with nothing
in the row looking wrong. Both write boundaries therefore REJECT surrounding
whitespace rather than canonicalizing it:
`validateLinkedInRefreshCredentials` (`internal/service/connection.go`, a 400) and
`validateConditionalGroups` (`internal/bootstrap/sysacct.go`, an install refusal).
The bootstrap site matters most: `requiredCredentialKeys[linkedin-ads]` is
`{"access_token"}` alone, so the refresh trio is reached ONLY through the
conditional-group loop, and the row it guards is the LF system account — the
fallback for every project with no connection of its own. Refusal is deliberate:
a credential is opaque to this service, so silently rewriting one would hide a
truncated paste. `fetchToken` also trims `ClientID`/`ClientSecret` as defence in
depth for rows written before those validators existed (`NewClient` already
trimmed `RefreshToken`). When refresh material IS
present the client exchanges it at LinkedIn's token endpoint before the access
token expires, coalescing concurrent callers single-flight (`tokenMu` is never
held across the network call) so N callers produce ONE exchange — the same
discipline as the google-ads and microsoft clients.

Because LinkedIn does NOT reset a refresh token's TTL when it is used, refresh
defers the 60-day access-token expiry but cannot remove the ~365-day deadline on
the connection itself; the client logs a warning inside the final 30 days. A
credential that is expired and unrefreshable — including a mid-flight 401, which
revocation can trigger with no advance notice — fails CLOSED with the exported
`ErrCredentialsExpired`, naming the connection so an operator knows what to
re-authorize, rather than degrading into an unauthenticated call.

**A rejected token exchange is classified on the OAuth `error` code, not on status
alone.** A token endpoint answers 400/401 for both a dead refresh token and a wrong
application credential, and the two have opposite remedies, so `fetchToken` parses
RFC 6749 §5.2's `error`: `invalid_client` is an operator misconfiguration and is
deliberately NOT `ErrCredentialsExpired`, because no member re-authorization can
repair a wrong `client_id`/`client_secret`. Everything else on a 400/401 — including
an absent or unparseable code — keeps the expired/revoked/invalid reading. Only the
parsed code is matched against a local constant; `error_description` is never read
and no upstream byte reaches the message.

**Splitting the arm is only half the job: it must still be CLASSIFIABLE.** Every
consumer of this classification matches structurally (`errors.Is`), so an
`invalid_client` returned as a bare `fmt.Errorf` unwraps to nothing and is invisible
to all of them — it carries neither a reason sentinel nor `ErrConnectionNotUsable`
and falls through to the generic retryable 503, the same opaque surface the split
exists to retire. It is therefore its own typed error unwrapping to the exported
`ErrApplicationCredentialsInvalid`. The dispatch layer re-tags an expired credential
as `ErrConnectionNotUsable` + `ErrCredentialsExpired`, and a rejected application
credential as `ErrConnectionNotUsable` + `domain.ErrApplicationCredentialsInvalid`:
the shared sentinel keeps BOTH out of the retryable bucket and out of an opaque 500,
while the distinct reason carries the remedy — the member re-authorizes for one, the
operator who configured the connection corrects it for the other. No token, secret
or upstream body is ever logged or echoed into an error.

**A 401 answering a MUTATING request states two facts, not one.** "Reconnect this
connection" is the actionable half, but LinkedIn "reserves the right to revoke
Refresh Tokens or Access Tokens at any time", so a revocation can take effect
between LinkedIn committing a create POST and writing its response — which leaves
the create outcome genuinely UNKNOWABLE, the same position a mutating 5xx leaves
the caller in. `credentialsExpiredError` therefore carries `Method` and
`StatusCode` (never rendered by `Error()`; they exist only for classification),
and `createOutcomeAmbiguous` reads them under the SAME method gate the
`transportError`/`apiError` arms use. So a POST/PUT/PATCH/DELETE 401 is ambiguous
("a campaign group may exist — verify by name"), while a GET 401 created nothing
and stays a definite expiry. The PRE-SEND expiry arms (a known-past access-token
expiry, an expired refresh token, a rejected token exchange) leave both fields
ZERO, and a zero method is not a mutating method, so they stay definite failures
without a special case. Before this, the 401 arm discarded the in-scope method and
status, so a create cascade interrupted by a mid-flight revocation returned a clean
`(nil, err)` — the dispatcher took its `result == nil` arm, RELEASED the dispatch
claim, and a retry could orphan a second billable campaign group that nothing was
told to reconcile. Fail-closed is unchanged: the rejected request is still never
replayed inside the failing operation (see `invalidateAccessToken`).

**A 401 is classified on EVERY arm that can observe one, not only the arm that read
the body.** `doRequest` can leave a non-2xx response by three different exits — a
readable body, a body-read failure (a mid-flight reset after the status line), and a
body over `maxResponseBytes` — and the two unreadable-body exits return BEFORE the
status arm. `expiredCredentialsError` is therefore the single construction site all
three call: it returns `nil` when the status is not an expiry, so each arm uses it as
a guard ahead of its generic `apiError` return. The body argument is OPTIONAL (`""`
from the arms that never obtained one), because `isTokenExpiryResponse` uses
`serviceErrorCode` only as a positive signal and already treats an unparseable body as
an expiry — the 401 status alone is the operative signal. Concentrating it there is
what keeps the two halves inseparable: an unreadable 401 previously fell through to a
bare `apiError`, which (a) skipped `invalidateAccessToken`, leaving a token LinkedIn
had already rejected in cache for the next caller to replay, and (b) hit
`createOutcomeAmbiguous`'s `apiError` arm, whose status list covers only 3xx/429/5xx —
so a mutating POST answered 401 read as DEFINITE and released the dispatch claim, the
exact harm the method/status plumbing above exists to prevent. The `skipBody` (status
update) path is NOT exempt: its early returns are gated on a 2xx, so a non-2xx falls
into the same arms, and the cascade tunnels `PARTIAL_UPDATE` over POST. `metrics.go`'s
parallel request path shares the construction site for the cache-invalidation half; its
ambiguity classification is a no-op there because the method is a hard-coded GET, which
the method gate correctly keeps definite.

**Every error arm of `fetchToken` rebuilds its cause from CLASSIFICATION rather than
forwarding text**, because the token exchange is the one request whose body carries
`client_secret` and the refresh token and whose 2xx response carries the rotated refresh
token — and these errors persist into a campaign's `Steps`. The transport arm goes through
`redactHTTPDoError`, the body-read arm through `redactBodyReadError` (both preserving
`context.Canceled`/`DeadlineExceeded` for `errors.Is`), the non-2xx arm reports status only,
and the 2xx decode arm reports `malformed JSON (%d bytes)` rather than wrapping the
`encoding/json` cause. That last arm matters less than the others and is defence in depth
rather than a demonstrated leak: `json.UnmarshalTypeError` reproduces an out-of-range NUMBER
literal verbatim and unbounded, but a credential is not all-digits and any non-numeric byte
fails as a syntax error first, so no secret is expressible through it. It is redacted anyway
so the rule holds uniformly — `WithHTTPClient` accepts an arbitrary `RoundTripper`, which
makes both the transport error and the response it returns caller-controlled. The sibling
decode in `metrics.go` does the same for the same reason.

`CreateCampaign` builds the full sponsored-content hierarchy in
one call — Campaign Group (ACTIVE) -> Campaign (PAUSED) -> Dark Post
(`feedDistribution: NONE`) -> Creative — with targeting assembled from the
runtime config's profile (skills/groups/job-functions) and resolved geo URNs.
Cross-tenant org/account pairing fails closed. `GetCampaignMetrics` reads live
campaign analytics from LinkedIn's Ad Analytics API; the dispatcher's `ReadMetrics`
method implements the optional `service.MetricsReader` interface the orchestrator
discovers per dispatcher and delegates to this client method.

A deliberate divergence from the TS source is that geo resolution is a pure,
cache-only function (no network fallback). Beyond that, the Go port validates
strictly and fails BEFORE any permanent resource is created: budget minimums and
sub-cent/NaN/Inf; registration URL (absolute, http/https, real host); schedule
(malformed/past/reversed); event name and project (trimmed, length-bounded);
targeting facet URNs (numeric ids in the correct namespace); ad-account and org
ids (numeric); geo URNs; and the aliased `cloud-native` profile must exist for
`custom`. Find-or-create uses cursor pagination and propagates transient search errors
(rather than treating them as "not found") to reduce duplicates — including a response with no
`metadata` block, which on a NO-MATCH page is refused rather than read as exhaustion. That
refusal is the expensive half of the same guard: this walk's absence value is the licence to
create, so a dropped cursor envelope on an intermediate page would answer "no such campaign" for
a name sitting on a page never fetched, and the caller would create a DUPLICATE PAID CAMPAIGN.
A page that CONTAINS the match returns its id without consulting the envelope — the guard sits
after the element scan on purpose, because a hit is not an absence and no unread page could
change it. It is
best-effort and NOT atomic across calls: `CreateCampaign` re-POSTs every dark
post and creative on a repeat call, so this package does not itself guarantee
cross-call idempotency. Single-flight IS provided caller-side by the orchestrator's
per-(brief, platform) claim, but that claim is held until explicitly released and is
NOT reclaimed on a timer: a crashed holder strands it until a human acts, which
`StuckDispatchClaims` surfaces (see internal-dispatch.md). Provider-level idempotency
KEYS remain unimplemented (LFXV2-2665). A 429 (idempotent methods only) is retried
with bounded backoff.

`CampaignResult` carries `accountId` (LFXV2-3050), stamped in `buildResult` from the account the
create ran under, so the dispatcher's provenance guard can refuse a toggle or metrics read whose
connection has since been re-pointed to another sponsored ad account. Rows written before the
field existed stay checkable: the guard falls back to parsing the account out of `linkedInUrl`,
which `campaignManagerURL` builds as `/campaignmanager/accounts/<id>/campaigns[/<id>]`. See
`internal-dispatch.md` for the guard itself.

## Campaign status toggle

`UpdateCampaignAndCreativesStatus(ctx, campaignID, status)` pauses/resumes a campaign AND
cascades to its creatives, because CreateCampaign leaves creatives DRAFT (and the campaign
PAUSED) — activating only the campaign would not serve (a DRAFT creative never serves; a
creative's EFFECTIVE status is gated by its campaign). Ordering is STATUS-DEPENDENT so a
partial cascade never leaves paid delivery running: on ACTIVATE the creatives are lifted FIRST
(still gated by the paused campaign, so they can't serve yet) and the campaign is flipped
ACTIVE LAST — a creative failure then leaves nothing serving; on PAUSE the campaign gate is
flipped FIRST (delivery stops immediately) and the creatives paused after. Each PARTIAL_UPDATE
is `POST /adAccounts/{acct}/{adCampaigns|creatives}/{id}` (header `X-Restli-Method:
PARTIAL_UPDATE`, body `{"patch":{"$set":{"status"|"intendedStatus": …}}}`). Creatives are
DISCOVERED via the creatives FINDER (`GET /adAccounts/{acct}/creatives?q=criteria&campaigns=List(urn:li:sponsoredCampaign:{id})`,
`X-Restli-Method: FINDER` — LinkedIn persists only a creative count, not ids). Each finder id
(URN or bare-numeric) is normalized to its trailing numeric id and the URN is REBUILT from it,
so a malformed value (a suffix with `?`/`/`/percent-encoding/traversal) is rejected rather than
altering the request path; the URN key is then percent-encoded (`urn%3Ali%3A…`). A stuck/looping
cursor, the page cap, an element with no usable id, or a response carrying elements with NO
`metadata` block at all FAILS discovery rather than truncating. That last case matters because
an absent cursor envelope and an exhausted cursor decoded to the same empty token before
`linkedInMetadata` became a pointer, so a truncated walk reported itself as complete.
On a PAUSE a definite 400 on an in-review creative is tolerated (LinkedIn forbids pausing an
in-review creative; the campaign gate already stopped it). Any other failure after a first
successful mutation is a `partialCascadeError` (Unconfirmed). The narrower `UpdateCampaignStatus`
(campaign only) is retained as the building block. The account id is resolved+validated from
the runtime config (same as create); ids must be numeric. `IsOutcomeUnconfirmed(err)` exposes
the shared ambiguity classifier (and honors the `Unconfirmed()` behavioral interface) so a
caller can tell a maybe-applied outcome (including a partial cascade) from a definite rejection.
`doRequest` gained an optional per-call headers map to carry the `X-Restli-Method` header.

## Metrics read

`GetCampaignMetrics(ctx, accountID, campaignID, window)` is the platform-client helper behind
`dispatch.LinkedInDispatcher.ReadMetrics` — the dispatcher, not this method, is what satisfies
`service.MetricsReader` (the type-asserted, optional capability the orchestrator's live-read
metrics endpoint discovers per dispatcher — see `internal/service/orchestrator.go`); the two
signatures differ. `campaignID` is the BARE
NUMERIC id persisted by `campaignFromLinkedIn` (`trailingID` of the creation response's
campaign URN, not a URN) — this method builds the `urn:li:sponsoredCampaign:{id}` and
`urn:li:sponsoredAccount:{acctID}` URNs the Ad Analytics finder itself requires.

The same Rest.li-vs-transport encoding split applies to the find-or-create name filter, and
`doRequest` handles it inline rather than by bypass. `restliEncode` produces the FINAL bytes
for a name embedded in a Rest.li literal — the COMPLETE query component via `url.QueryEscape`
with `+` rewritten to `%20`, the caller supplying the `(name:(values:List(` … `)))` syntax raw
— and `buildRawQuery` writes any parameter whose value came from `restliEncode` (the set is
`preEncodedParams`) to `RawQuery` verbatim. An enumerated escape list shipped first and missed
`&` and `#`, which truncate the query at the URL layer rather than inside the literal.
Both halves are load-bearing: `url.Values.Encode()` renders a space as `+`, which the
Rest.li parser reads as a
literal plus and rejects with `400 PARAM_INVALID`, while re-encoding an already-encoded value
turns `%20` into `%2520`, which matches a literally-different name and returns a
**clean-looking empty result set** — a false "not found" that drives a duplicate paid create.
Assertions about this encoding must read `r.URL.RawQuery`, never `r.URL.Query()`: the latter
percent-decodes, so a correct value and a bare one are indistinguishable through it.

The Ad Analytics finder (`GET /adAnalytics`) uses Rest.li 2.0 array/nested-object query
literals — `dateRange=(start:(day:D,month:M,year:Y),end:(...))`, `campaigns=List(urn:...)`,
`accounts=List(urn:...)` — that are not expressible through `url.Values.Encode()` (which would
percent-encode the structural parentheses/colons LinkedIn requires literally), so
`makeAdAnalyticsRequest` builds the raw query string directly and bypasses `doRequest`'s
flat `map[string]string` param model entirely, going through a dedicated
`doAdAnalyticsAttempt` GET path instead. It reuses the client's existing 429 retry policy
(`parseRetryAfter`/`retryBaseDelay`/`maxRetryWait`/`sleepCtx`), the same as `doRequest`'s
idempotent-method retry rule. **UNVERIFIED ASSUMPTION**: the finder name (`q=analytics`),
`pivot=CAMPAIGN`, and `timeGranularity=ALL` are LinkedIn's documented Ad Analytics contract,
flagged in code but not yet verified against a live Marketing API account.

The response's `elements` field is decoded via a `*[]AdAnalyticsElement` pointer so a
missing/null field (malformed response — empty body, `{}`, `null`) is distinguishable from an
explicit `{"elements": []}` (genuine zero activity in the window): a nil pointer is a decode
error, never silently reported as zero metrics. Spend (`costInUsd`, decimal USD returned as a
JSON string representing a BigDecimal) is parsed with `big.Rat`, NOT `strconv.ParseFloat`: a
BigDecimal can carry more precision than float64 holds, so a float parse would round the
value twice. `costInUsdToMicros` multiplies the exact rational by 1e6, rounds — not truncates
— to the nearest integer, and rejects the result unless `big.Int.IsInt64` confirms it fits,
so an out-of-range spend is an error rather than a silently wrapped micro value.

`dateRangeForWindow` anchors both `this_month` and `last_month` off the first-of-month date
rather than `AddDate(0, -1, 0)` on today's day-of-month, since `time.AddDate` silently
normalizes an invalid day (e.g. subtracting a month from the 31st) into the following month —
that would otherwise shift both windows' boundaries on 29th/30th/31st-of-month days.

## Ad-account discovery

`ListAdAccounts` (`accounts.go`) enumerates every ad account the client's access token can
reach, so a connection that holds only credentials — or one being re-pointed at a different
account — can ask which accounts are available. The request is `GET /adAccounts?q=search`
with the `search` criteria OMITTED, which LinkedIn documents as returning every account the
caller has access to; no account id appears anywhere in the path or parameters, which is what
makes it callable before an account has been chosen. `newAccountsClient` in the test builds a
client with a deliberately ZERO `RuntimeConfig`, so a future edit that starts consulting a
configured account fails in the test rather than in production.

The two required headers cost nothing here: `doRequest` already sends `LinkedIn-Version` and
`X-RestLi-Protocol-Version: 2.0.0` on every call. Nor is there a no-`elements` guard in the
walk beyond a defensive nil check — `doRequest` already fails any GET whose body cannot prove
a result set. Restating either contract locally is exactly the drift risk the id check below
avoids.

**Two health axes, kept apart.** An ad account's lifecycle `status` (ACTIVE, CANCELED, DRAFT,
PENDING_DELETION, REMOVED) and its `servingStatuses` array answer different questions and can
disagree: an ACTIVE account on BILLING_HOLD is perfectly bindable but will not spend. So
`Active()`/`StatusLabel()` report the lifecycle and `Servable()`/`ServingHolds()` report the
serving state, rather than collapsing into one verdict that would either hide a usable account
or promise one that cannot serve. `Servable()` is an ALLOW-LIST — exactly `["RUNNABLE"]`, and
not a test account — so an absent or unrecognized value reads as "not confirmed servable"
rather than as healthy. The test-account term is not redundant: LinkedIn reports `RUNNABLE` on
test accounts, because they *are* runnable in the sense that field means, while a campaign
bound to one never serves. Without it, a picker built on `Servable()` would present a test
account as the one healthy choice — the most misleading answer available to it.
`ServingHolds()` stays empty in that case, which is how a caller tells "held" from "unknown". An absent lifecycle `status` is likewise not a claim either way: not `Active()`, and no label.

`test` accounts are surfaced through the `Test` field rather than filtered. A test account
never serves and never bills, so binding a real campaign to one produces a campaign that
silently does nothing — but a developer wiring up an integration is looking for precisely that
account, so the honest move is to show it and say what it is — surfaced, but never reported
as servable. Same reasoning applies to
canceled, draft and held accounts: this feeds a picker, and dropping a user's only account
answers "your token reaches no ad accounts" about an account sitting right there.

**Fail, do not truncate.** A repeated page cursor, an ABSENT `metadata` block, and the page cap
(`adAccountPageSize` x `adAccountMaxPages`) all return an error rather than the accounts
collected so far, because a short list is indistinguishable from a complete one at the boundary
and the caller acts on the absence. The metadata case is the least obvious of the three: a
response carrying `elements` but no cursor envelope decodes to an empty `nextPageToken`, which
reads exactly like exhaustion, so the walk stops and reports a partial enumeration as complete.
The field is therefore a POINTER — absence has to be representable before it can be rejected.
The cap is 20 pages, far tighter than `maxListPages`: that constant exists so a
find-by-name survives a server-side filter the API may ignore, and discovery has no filter to
be ignored, so a walk that long means something is wrong rather than that the collection is
large.

**The cursor is echoed back byte for byte**, and is one of the few strings in this walk that
is NOT trimmed. It is an opaque server token, so trimming can request a different page than
the one offered; worse, a token consisting only of whitespace would trim to `""`, read as
exhaustion, and return page one alone as the complete account list — the same false absence
the guards above exist to prevent, arriving through the pagination door instead. The two
older cursor walks in `client.go` (creative discovery, find-by-name) already preserve the
exact value. Trimming belongs on human-entered fields such as the account NAME, not on
anything the server minted.

The id check reuses `accountIDRE` from `targeting.go` — the same regexp a configured account
id is validated against — rather than restating `^[0-9]+$`. An account this walk offers must
be one the client will later accept, and a second copy of that contract could drift into
offering ids that fail at bind time. An unusable id fails the WHOLE walk rather than skipping
the row: a response shape that far from the documented one is not the response we think it is,
and the rest of it is not trustworthy either.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` linkedin adapter (see [internal/dispatch](internal-dispatch.md))
interprets a single OAuth2 accessToken; it builds RuntimeConfig from the connection's
AccountID + `org_id` (must be the NUMERIC org id) plus caller-supplied targeting profiles
from config.

It implements `StatusToggler` and CASCADES: its create leaves the campaign PAUSED and its
creatives DRAFT, so a full ACTIVATE must lift the creatives too (a DRAFT creative never
serves, and a creative's EFFECTIVE status is gated by its campaign).
`UpdateCampaignAndCreativesStatus` PARTIAL_UPDATEs the campaign status, DISCOVERS the
creatives via the creatives FINDER (LinkedIn persists only a creative count, not ids), and
PARTIAL_UPDATEs each creative's `intendedStatus`. On a PAUSE, a definite 400 on an
in-review creative is tolerated (LinkedIn forbids pausing an in-review creative) — the
campaign is already the effective gate.

See [internal/platform/linkedin](../../../internal/platform/linkedin).
