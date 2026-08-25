---
type: "Go Package"
title: "internal/platform/googleads"
description: "Google Ads API REST client: OAuth2 refresh-token auth, request layer with 429 retry, GAQL search (GA-1), PAUSED campaign creation via campaignBudget→campaign :mutate with the no-idempotency-key ambiguity contract (GA-2), Responsive Search Ad copy generation + redacted final-URL building (GA-3a), ad group + responsive search ad creation (Campaign->AdGroup->Ad, create-then-catch-duplicate idempotency, composite AdGroupAd resourceName) (GA-3b), a dispatcher-level status-toggle cascade over that ad group/ad (GA-3c), keyword/audience-segment targeting on that ad group via adGroupCriteria:mutate, with ad-group-level targetingSetting keeping audience criteria observation-only rather than restrictive (GA-4), read-only campaign metrics via GAQL googleAds:search with a validated campaign id and window allow-list (GA-5), ad-account discovery — customers:listAccessibleCustomers plus manager (MCC) hierarchy expansion via customer_client, on an account-agnostic request path that validates only the manager id so a caller with no customer id yet can still enumerate; geo/location targeting from ISO alpha-2 country codes resolved to Google geo target constants, attached at campaign level for Search and ad-group level for Demand Gen (LFXV2-3283); project-scoped keyword-performance and age/gender/device audience reads plus atomic pause/remove keyword actions over adGroupCriteria:mutate, with a truncation-signalling row cap, per-dimension bucket aggregation, and resource-name verification on every applied mutation (LFXV2-2641); and a read-only campaign settings readback via GAQL with campaign_budget attributed from campaign, whose every field is optional so a setting Google did not return stays ABSENT rather than defaulting to zero (LFXV2-3067)."
resource: "internal/platform/googleads"
tags:
  - platform-client
  - google-ads
  - oauth2
  - gaql
  - go-package
timestamp: "2026-07-18T00:00:00Z"
---

# internal/platform/googleads

Package googleads provides a Go client for the Google Ads API, ported from the
upstream TypeScript `google-ads-api` gRPC usage (`campaign-proxy.service.ts` /
`campaign-metrics.service.ts`) to a **REST** client that speaks the Google Ads
REST transport directly (`googleAds:search` / `:mutate`). REST — rather than the
official gRPC SDK — is deliberate so the client matches the meta/reddit/twitter/
linkedin clients' structure and avoids a large generated gRPC dependency.
Credentials and account configuration are injected via `NewClient`; the client
never reads the process environment.

## Auth

Google Ads auth is richer than a single Bearer token. `Credentials` carries the
OAuth2 `client_id`/`client_secret` + `developer_token` + `refresh_token`;
`AccountConfig` carries the `customer_id` (digits only) and an optional
`login_customer_id` (manager/MCC access). Every call sends the bearer access
token, the `developer-token` header, and — when set — the `login-customer-id`
header. The refresh-token→access-token exchange is coalesced with a single-flight
leader/follower (the token mutex is never held across the network call, and the
refresh runs on a `WithoutCancel`-detached context so one caller's cancellation
can't tear down a shared refresh). The OAuth response body is never echoed into
errors (it can carry the `client_secret`/`refresh_token` back).

A resource-server **401 invalidates the cached access token**, on the STATUS LINE and before
the response body is read. Expiry alone is not enough: a revoked or rotated token keeps its
advertised `expires_in`, so the fast path would go on serving it long after the platform
started rejecting it. That only became reachable once the dispatcher began caching CLIENTS
across operations — a client rebuilt per operation started with an empty cache, so one
rejection cost one failure. Reading the body first would buy no accuracy (a 401 is a 401
whether or not its body arrives) and would hold the rejected token available to every
concurrent caller for the rest of the attempt timeout, so the unreadable and oversized arms
need no guard of their own.

Invalidation is **compare-and-clear**, not an unconditional clear: it takes the token the
rejected request actually presented and drops the cache only if it still holds that token.
With a shared client, request A can leave carrying `tok_1`, request B can refresh and cache
`tok_2`, and A's late 401 then names `tok_1` — clearing blindly would evict a token nothing
rejected and let a burst of late responses drive serial re-exchanges, defeating the
single-flight coalescing above. Matching makes it idempotent: the rejected token is dropped
exactly once, and every later 401 naming it is a no-op.

The cache is not the only place the rejected value lives, so a flight whose token MATCHES
the rejected one is blanked and unpublished as well — as selective as the cache clear, so a
flight carrying any other token still survives. Be precise about what that arm buys today:
on the CURRENT leader path it is NOT reachable. `accessTokenValue` sets `inflight.token`, retracts
`inflight` and closes `done` inside ONE unbroken critical section, so a published flight is
never observable holding a non-empty token — by the time the token is set, the flight is
already unpublished under the same lock hold. It is kept as defence-in-depth against that
critical section being split, not as live coverage, and it does NOT close the residual
pre-publication window tracked by #180: a 401 arriving BEFORE the leader sets `inflight.token`
matches nothing, and the leader then publishes the already-rejected token. Blanking without
unpublishing would recurse without bound (a waiter re-leads, finds the same poisoned flight,
and reads the blank again), and because invalidation can retract a flight early, the leader's
own teardown compares identity before clearing so a stale leader cannot erase a newer flight.

## Request layer

`doRequest` applies the repo's standard discipline: no-follow redirects
(unconditional, on a shallow copy so a supplied client isn't mutated), bounded
response reads (`maxResponseBytes+1`), and the pre-send (`isPreSendDialError`) vs
ambiguous (`transportError`) vs definite (`apiError`, status-only `Error()`)
classification. A rate-limited (429) **idempotent** call is retried up to
`retryMax` times with a bounded backoff honoring `Retry-After`; retry eligibility
is an explicit parameter (not the HTTP method) because GAQL `:search` is
POST-but-read-only — a `:mutate` create must NOT be retried (double-create risk).
`customer_id`/`login_customer_id` are validated as digits-only before any request.

## GAQL search

`gaqlSearch` runs a cursor-paginated GAQL query (POST) with a repeated-token guard,
a page cap, and BOTH an aggregate-row cap and an aggregate-byte cap so a many-page
result can't OOM the service. The byte cap counts each page's FULL raw payload
(not just result rows), so it also bounds the retained nextPageToken strings — a
row cap alone doesn't bound memory, so the byte cap is the real memory guard.

**GAQL gotcha:** in API v23, `campaign.start_date` / `campaign.end_date` were
replaced by `campaign.start_date_time` / `campaign.end_date_time` — the old fields
are rejected as unrecognized.

### Interpolating free text into a query

Until campaign lookup, every GAQL query here interpolated a digits-only id (`customerIDRE`) or
a closed allow-list value (`validMetricsWindows`), so nothing needed escaping. A campaign
**name** is the first genuinely caller-controlled string to reach a `WHERE` clause.
`gaqlStringLiteral` renders it single-quoted, escaping backslash **first** and then the quote —
the reverse order re-escapes the backslash the quote escape introduced and releases the quote.

It rejects only **NUL, LF and CR**, exactly what Google Ads prohibits in `Campaign.name`. A
blanket "reject every control character" rule is wrong here and was corrected in review: this
lookup serves **adoption**, whose targets never passed through `sanitizeNamePart`, and Google
accepts TAB, U+2028/U+2029 and zero-width joiners — rejecting one answers "no such campaign"
about a campaign that exists. (`unicode.IsControl` covers category **Cc only**, so the line
separators slip past it and invite an explicit check that over-rejects twice over.) Allowing
them risks nothing: the query rides in a JSON body that `encoding/json` escapes.

**Invalid UTF-8 is the one rejection that is not about what Google forbids.** It is about what
*this process* would change. `encoding/json` substitutes U+FFFD for each malformed byte and
returns **no error**, so a name carrying one is silently rewritten between the guard and the
wire: the query asks about a name the caller never passed, and its inevitable miss is reported
as the clean `("", nil)` absence that licenses a create. Nothing downstream catches it — the
rune loop sees `utf8.RuneError`, which is none of NUL/LF/CR; the length check counts it as one
rune; and the row-level name re-check needs a row, which a query that matches nothing does not
return. Rejecting costs no reachable lookup either, since Google Ads' JSON and proto surfaces
both require valid UTF-8, so no stored campaign name can contain a malformed byte. The general
form: *an encoder that lossily repairs its input is a silent query rewriter, and a fail-closed
lookup must validate against what will actually be transmitted, not what it was handed.*

**A repeated JSON key is a self-disagreement the decoder settles for you.** RFC 8259 leaves
duplicate object keys undefined and `encoding/json` keeps the LAST one, so a row reading
`{"id":"999","id":"555","resourceName":"customers/1/campaigns/555"}` decodes as campaign 555,
agrees with its own resource name, passes every identity guard, and is returned as a confirmed
lookup — while the same bytes also identify 999 to any reader following the equally-permitted
first-wins convention. Every other guard on these two paths exists to refuse a row that
contradicts itself; this is the one contradiction the decoder resolves silently rather than
reporting. `hasDuplicateKeys` therefore walks the raw bytes before `Unmarshal` on **both**
lookup paths, and it walks the WHOLE row, not just the fields this client reads: a duplicate
anywhere is evidence the producer is not emitting what we think it is, and the selected-field
set changes over time, so a guard scoped to today's fields would quietly stop covering
tomorrow's. Malformed JSON reports false and defers to `Unmarshal`, whose error is the better
diagnostic.

**Sameness is the decoder's, not the bytes'.** `encoding/json` prefers an exact tag match and
falls back to a CASE-INSENSITIVE one, so `{"id":"999","ID":"555"}` assigns the same field twice
and leaves 555 — two contradictory ids and no repeated key for a byte-comparing guard to see.
Keys are therefore folded (`foldKey`) before comparison, including the two runes the decoder
special-cases, KELVIN SIGN and LATIN SMALL LETTER LONG S, which simple-fold onto `k` and `s`.
Folding cannot over-reject here: Google's JSON is lowerCamelCase throughout, so no legitimate
object carries two keys differing only in case.

**The guards also run on the ENVELOPE, and there the reason is stronger.** The row checks run
on rows the envelope has already produced, so no row guard can see a corruption that destroys a
row on the way out. `{"results":[<campaign 555>],"results":[]}` is that corruption: last-wins
leaves zero rows, the guard loop never executes, and what reaches the caller is a clean,
trustworthy absence — the one answer a fail-closed lookup must never manufacture, because its
callers read an absence as a licence to create a real paid campaign. A duplicated
`nextPageToken` silently truncates or redirects pagination the same way. `gaqlSearchForCustomer`
therefore runs `utf8.Valid`, the surrogate scan and `hasDuplicateKeys` on the raw page before
decoding it, which covers every GAQL reader in the package rather than the two lookup paths.
Neither of the first two can over-reject a page: invalid UTF-8 bytes make the document malformed
per RFC 8259 §8.1, and Google Ads cannot store an unpaired surrogate in any field. The per-row
checks stay where they are — they name the campaign in their diagnostics, which the envelope
check cannot.

The name is queried **verbatim**, no `TrimSpace`: trimming is a no-op for the create path
(`composeName` already trims), so it only ever alters adoption, answering `"  foo  "` with the
campaign named `"foo"`. `TrimSpace` only *detects* whitespace-only input. Anything new
interpolating free text into GAQL must go through the helper.

## Campaign lookup by name

`FindCampaignByName` returns the id of the single live campaign with exactly that name. The
fail-closed logic mirrors the other clients' lookups (meta's `findCampaignByName`, linkedin's
`findMatch`, twitter's and microsoft's) because callers make the same decision from the
result. It is the first one **exported** — the others are called only from inside their own
create path; this one is exported because `GoogleAdsDispatcher.Dispatch` calls it before
creating, to adopt a campaign that already carries the composed name rather than create a
second paid one (LFXV2-3042).

Adoption is **opt-in** — `googleAdsConfig.adoptExisting`, default `false` — and the default
is a correctness property, not a convenience. `ComposeName` is deterministic in
Project/EventName/NameSuffix and does not change when the local campaign row is soft-deleted,
while `getCampaignByPlatformQuery` excludes deleted rows. So after a documented delete the
orchestrator reads the pair as "never dispatched" and re-enters `Dispatch`; an unconditional
lookup would there re-attach to the still-live upstream campaign the delete walked away from
and persist the new request's budget/config against it while pushing nothing upstream. With
the flag off, that dispatch goes down the create path, where Google's `DUPLICATE_NAME` is
surfaced as a retained partial requiring reconciliation — visible, which the silent rebind
is not. An adopted row is persisted `created_degraded`, never `created`: nothing was wired
upstream (no budget, no ad group, no ad, and not this request's config), the same reason
`twitter.go` degrades its `Reused` case.

| outcome | result |
|---|---|
| exactly one live (`ENABLED`/`PAUSED`) match | `(id, nil)` |
| no live match — only `REMOVED` rows, or none | `("", nil)` — a clean, trustworthy absence |
| more than one match | `("", error)` — ambiguous, never a silent pick |
| a row whose name is **not** the queried name | `("", error)` — the filter was not honoured |
| unrecognised status (`UNSPECIFIED`, `UNKNOWN`, empty) | `("", error)` |
| unverifiable (undecodable row, no usable id, non-canonical id, malformed or cross-customer resource name, identity fields that disagree) | `("", error)` |

**Why absence and ambiguity must differ.** The one caller acts destructively on an absence.
`("", nil)` is not reported upward as "nothing to adopt" — `Dispatch` **falls through to
`CreateCampaign`** on it, so a verified absence is a licence to create and a false absence
produces a **duplicate paid campaign**. An arbitrary pick binds a brief to the wrong one. Two live campaigns sharing a name is **anomalous, not routine** — v23 rejects a
mutate whose name another `ENABLED`/`PAUSED` campaign holds (`DUPLICATE_CAMPAIGN_NAME`) — so
this branch fail-closes on a response that should not be possible. Rows are deduplicated by id
first, so one campaign on several rows is not ambiguous.

The name filter and `REMOVED` exclusion are applied **server-side** (a miss costs one page
instead of an account walk) and **re-checked client-side** — not redundant, since an injected
query still returns 2xx rows for OTHER campaigns. **The disposition matters as much as the
check:** a name mismatch is an **error**, never a skip, because skipping every injected row
leaves `("", nil)` — *a skip that reduces an unverifiable response to a clean absence is a
false-absence bug.* Status is the deliberate asymmetry: `REMOVED` **is** a per-row skip,
because a tombstone is unadoptable — but only once you know it IS a tombstone of some
identifiable campaign. `campaignRowIdentity` validates the id and resource name BEFORE it
reports `live == false`, so "however it arrived" describes the STATUS, not the row: a
`REMOVED` row with a malformed, cross-customer or self-disagreeing identity is an error like
any other, because such a row is not evidence that anything was removed. Skippability is a
property of an identified tombstone.

Identity is validated **whenever present**, not only as a fallback: both fields are selected,
so both are evidence of what the row is, and a malformed or cross-customer resource name beside
a plausible id — or two fields naming different campaigns — errors. The shape check is the full
documented one (`customers/{this account}/campaigns/{digits}`), **not** `resourceID`, which
returns the trailing segment: right for a mutate response we issued, wrong here, where it reads
`garbage/4242` as `4242`.

Presence is tested on the **raw** field. Trimming it first would fold a whitespace-only
resource name into "field absent" and let the row be adopted on its id alone — a row whose two
selected identity fields do not agree, accepted as though only one had been asked for. Absent
and present-but-garbage are the distinction the guard exists to draw.

**Digits-only is not the id test.** Both ids go through `canonicalCampaignID`, which requires
the canonical base-10 spelling of a **positive int64** — the type Google exposes campaign ids
as. `customerIDRE` (`^[0-9]+$`) is the package's *interpolation-safety* matcher and passes
`"0"`, a value past `math.MaxInt64`, and `"007"`. The last is the one with teeth: `"007"` and
`"7"` are one campaign to the server and two strings to the identity comparison, so a string
compare reports a disagreement that does not exist — or an agreement between two spellings only
one of which is real. Canonicalising collapses the spellings, so the comparison is about
campaigns. `campaign.id` is also **not** trimmed: a padded value is a malformed row, and
`TrimSpace` would answer with campaign `4242` for a response this API does not produce.

A JSON `null` is refused at **three** levels: `gaqlSearch` rejects a top-level `null` body,
`results` decodes through `searchRows`, and `nextPageToken` decodes through `pageToken`. Each
would otherwise unmarshal without error into a zero-valued field, indistinguishable from a
genuine response, therefore a licence to create. An **omitted** key stays legal at every
level: `{}` and `{"results":[]}` are Google's own empty page, and a final page omits the
cursor.

The cursor case fails by a different route and is worth stating separately. `"nextPageToken":
null` decodes to `""`, which is exactly the value the pagination loop reads as "that was the
last page". The result set is not empty and is not misread as empty — it is read as
COMPLETE, and paging stops at page 1. A campaign on page 2 is then reported absent, and
`FindCampaignByName` treats absence as a licence to create a duplicate PAID campaign. Same
false absence as the other two, reached by silent truncation rather than by an empty page.
proto3 JSON emits an unset string as `""` or omits it and never emits `null`, so refusing it
costs nothing a conformant server would send.

## Campaign lookup by id

`GetCampaign` is the by-id counterpart of `FindCampaignByName`, and exists for **verify
before bind**: an operator supplying a campaign id is shown what that id resolves to *in this
account* before any binding is written, rather than having the id stored and the mismatch
discovered at dispatch time. It returns a `CampaignRef{ID, Name, Status}` — the name and
status are there because the decision is a human one; an id alone is not confirmable.

| outcome | result |
|---|---|
| one live (`ENABLED`/`PAUSED`) campaign with that id | `(ref, nil)` |
| no such campaign, or only a `REMOVED` one | `(nil, nil)` — a clean, trustworthy absence |
| a row for a **different** campaign | `(nil, error)` — the id filter was not honoured |
| the same id returned twice with different details | `(nil, error)` |
| unverifiable (undecodable row, unrecognised status, bad identity fields) | `(nil, error)` |

A `REMOVED` campaign reads as an absence here exactly as it does by name: the id names a real
record, but not one a brief can be bound to, and "you cannot adopt this" is what the caller
needs either way.

**The row-level checks are shared, not duplicated.** `campaignRowIdentity` answers "which
campaign is this row, and is it adoptable" for both entry points. Duplicating them would let
the by-id path become the lenient one, which is the worse direction to drift — a caller
handing over an id is about to attach real spend. `live == false` is returned for `REMOVED`
alone, the one skippable state; every other unrecognised status is an error, per the
enumerate-and-default-deny rule above. And it is returned only AFTER identity is
established — status is read last, so a tombstone whose identity does not check out errors
rather than skipping. Callers may rely on `live == false` meaning "this identified row is
unadoptable", never "there was something here, unclear what".

**The caller's id is validated as an identity, not merely as safe text.** `canonicalCampaignID`
runs *before* interpolation, so `"0"`, a value past `math.MaxInt64` and the leading-zero
spelling `"007"` are all refused despite being digits. `"007"` is why rejecting beats querying:
it matches campaign 7 server-side and would then trip the filter-not-honoured check, reporting
a confusing conflict for what is really a malformed request. `campaign.id` is an int64 in GAQL,
so the value is compared **unquoted** — quoting it would make this a string comparison against
a numeric field — and no escaping question arises, because the value has been proven to be
nothing but digits.

`GetCampaign` deliberately does **not** report whether the campaign is already bound to some
other brief. That is this service's own state, not Google's, and answering it from here would
be answering a database question with an ad-platform call.

## Campaign creation (GA-2)

`CreateCampaign` (in `campaign.go`) creates a PAUSED search campaign as two
sequential `:mutate` calls: `campaignBudgets:mutate` (a non-shared `STANDARD`
budget, `amountMicros` = budget × 1,000,000) then `campaigns:mutate` (status
`PAUSED`, `advertisingChannelType` `SEARCH`, referencing the budget's
`resourceName`, with a dependency-free `manualCpc {}` bidding strategy — a broker
can't assume conversion tracking, which `maximizeConversions` requires). The
`CampaignInput.Budget` field is denominated in the ad ACCOUNT's currency, NOT USD —
Google interprets the resulting `amountMicros` in the account's own currency and
the client does no FX conversion, so a value of 50 is 50 of whatever the account is
denominated in. (The field was renamed from `BudgetUSD`, which implied a conversion
this client does not perform; mirrors the meta client's `Budget`.) The
campaign create also sets `containsEuPoliticalAdvertising:
DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` — v23 REQUIRES this on every create
(omitting it fails with `FieldError.REQUIRED`, and since 2026-04-01 an account with
any undeclared campaign has ALL mutate calls rejected). It also sets
`networkSettings` with `targetGoogleSearch: true` (search/content networks
explicitly false): a SEARCH campaign that targets NO network — which is what an
omitted `networkSettings` resolves to (proto3 bools default false) — is rejected with
`CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK` AFTER the budget mutate has
committed, an avoidable orphan. Google Search only is the conservative choice for a
PAUSED broker shell; `targetSearchNetwork` stays false because true would opt into
Search Partners (and requires `targetGoogleSearch`), which a generic broker shouldn't
assume. Both resource ids are
surfaced (`campaignBudgetId` + `campaignId`) via `firstResourceName`, which decodes
`results[0].resourceName` and returns both the resource name and its trailing-id
segment. It errors when the body is malformed, carries no result/resourceName, OR
the resourceName is present but MALFORMED (e.g. `customers/1/campaigns/` or
`noslash`) such that no id can be extracted — accepting a present-but-malformed
name would let creation continue with an empty, unreconcilable id (or report
success with a blank id), so it is treated as UNCONFIRMED like the no-resourceName
case. Between the two calls, if the
caller's context is already done, the campaign `:mutate` is skipped and the created
budget is returned as a reconcilable partial rather than fired on a dead context.

Input is validated up front, before any paid `:mutate` call: the budget must be
finite (NaN/Inf rejected — NaN passes every ordered comparison, so it would
otherwise slip through and create a zero-unit budget) and must round to a positive
`amountMicros` (a sub-micro value like 0.0000001 is > 0 but converts to 0 micros);
and BOTH Project AND EventName must be non-empty (independently — mirrors the
meta/twitter/reddit clients). `CampaignInput.EventSlug` (plus the newer
`RegistrationURL`/`Headlines`/`Descriptions` fields) feeds the GA-3 ad-group/ad
step below — the campaign `:mutate` itself still ignores them. Project is the canonical attribution key the data
pipeline parses out of the campaign name, so a campaign with only one segment is
mis-attributed, not just "less descriptive". Caller-supplied name segments are
sanitized (`sanitizeNamePart`) to strip the `|` delimiter AND any control character
(incl. NUL — v23 forbids NUL/LF/CR in `Campaign.name`; `strings.Fields` only folds
whitespace control chars, so NUL is mapped to a space explicitly) before composing,
so a raw `|` can't inject extra pipe-fields (breaking name-based
attribution/reconciliation that splits on `|`) and a control char can't reach a paid
`:mutate` as a guaranteed-invalid name.

The composed name is length-validated against **per-entity** limits IN THEIR OWN
UNITS, not a single shared number (verified against the v23 System Limits table +
RPC field references): `CampaignBudget.name` is 1..255 UTF-8 BYTES (`len`), and
`Campaign.name` is up to 256 CHARACTERS (`utf8.RuneCountInString`,
`StringLengthError.TOO_LONG`). The unit difference is load-bearing — a multibyte name
hits the budget's byte ceiling sooner than 256 characters — so `validateEntityName`
is told the measured length and unit per name. Both are validated before either
`:mutate`; the budget name is composed+checked first, so its 255-byte cap is the
binding preflight guard for an ASCII name, and an oversized name never wastes a paid
call or orphans a budget.

Because `:mutate` has NO idempotency key, a blind retry double-creates. So every
create outcome is classified: an ambiguous failure (a mutating 3xx/5xx `apiError`,
a mutating **429** — `doRequest` deliberately does NOT retry a non-idempotent 429
because the throttled request may already have committed — a `transportError`, or a
2xx with no `resourceName`) is reported UNCONFIRMED (verify before retrying) with a
partial result carrying whatever exists so an orphan is reconcilable. The partial
carries BOTH deterministic names (`CampaignName` and `CampaignBudgetName`) alongside
any known ids. Before attachment the two names DIFFER (`LFX | Budget | …` vs `LFX |
Search Campaign | …`), so a caller reconciling a possibly-orphaned BUDGET **at the
budget stage** (no id yet) uses `CampaignBudgetName`. IMPORTANT: once the campaign
attaches, a non-shared (`explicitlyShared=false`) budget's name SYNCHRONIZES to the
campaign name — so at a CAMPAIGN-stage ambiguous failure the budget's current name is
unknown (it may already be `campaignName`). That is exactly why the budget-stage
partial also carries `CampaignBudgetID`: past attachment, reconcile the budget by ID,
not by its original name. A definite 4xx means only THAT `:mutate` was rejected — for a
campaign-create 4xx the budget from the first mutate still exists, so the partial
carries `campaignBudgetId` for reconcile/cleanup (it is NOT "nothing was created"
overall). Before the FIRST mutate, an already-cancelled context is caught explicitly
(`ctx.Err()`) and returns a clean `(nil, err)` — otherwise, with a cached OAuth token
the cancellation would only surface inside `httpClient.Do` as a `transportError` and
be mis-reported as UNCONFIRMED, implying a budget might exist when nothing was sent.
`createOutcomeAmbiguous` (5xx/429/transport always; 3xx only on a mutating method),
`isDuplicateBudgetNameErr` (a 4xx `CampaignBudgetError.DUPLICATE_NAME`), and
`isDuplicateCampaignNameErr` (a 4xx `CampaignError.DUPLICATE_CAMPAIGN_NAME` — a
DIFFERENT code from the budget's) drive this: a retry with a stable `NameSuffix`
that collides on the family-appropriate duplicate code is surfaced as
already-exists (reconcile by name). Error codes are parsed from
`error.details[GoogleAdsFailure].errors[].errorCode` (a single-key category→enum
object) from the FULL error body in `doRequest` and retained on
`apiError.ErrorCodes` — NOT re-parsed from the truncated `apiError.Body` (a real
Google error JSON exceeds `maxErrorBodyChars`, so parsing the truncated snapshot
would silently drop the codes). Codes are length/count bounded; the raw body is
never surfaced by `apiError.Error()`. Composed names are deterministic (`LFX |
<kind> | Project | Event | NameSuffix`) so a retry with the same `NameSuffix`
collides on the duplicate code rather than double-creating. NOTE: a non-shared
budget's name syncs to the campaign name once attached; Google does NOT document
that the original budget name is then freed for reuse, so for at-most-once retries
callers should pass a stable-per-logical-campaign `NameSuffix` (which makes the
retry collide on `DUPLICATE_NAME`) rather than relying on name reuse.

## Responsive Search Ad copy + final URL (GA-3a)

`ad_copy.go` holds the pure, side-effect-free helpers GA-3's ad-group/ad
creation cascade consumes: `composeAdCopy` resolves caller-supplied
headlines/descriptions into a valid RSA content set (trimmed, weight-capped —
CJK/full-width runes count double, see `truncateWeighted` — deduped, padded
with deterministic `eventName`/`project`-derived placeholders
up to the minimum, capped at the maximum — never silently dropping down to
zero, but empties/duplicates ARE dropped and the count IS capped, so a caller
should not assume its exact input list survives verbatim), and
`buildAdFinalURL` builds the ad's destination URL from the brief's
registration URL, UTM-tagging it: `utm_source`/`utm_medium` are set
unconditionally to `google`/`cpc` (a Google CPC click must attribute to this
channel, never to a `utm_source` the registration URL already carried), while
`utm_campaign`/`utm_content` are set only when the registration URL does not
already carry them (`setIfAbsent`).

`buildAdFinalURL` rejects a registration URL that: fails to parse; uses a
scheme other than http/https; has no host; carries embedded userinfo
(`user[:password]@host` — an ad destination never needs URL credentials, and
forwarding them downstream would leak a basic-auth secret); or has a
malformed query string (an unparsable percent-escape in `RawQuery` is
silently dropped by `u.Query()`, which would otherwise alter the actual
destination the ad points to). Every validation error redacts the raw
registration URL via `redactURLForError` (scheme+host+path only) before
including it in the message — the URL may carry a token in its query string
or userinfo, and this error can be logged or persisted in a result
step/snapshot. Mirrors the twitter client's `redactURLForError` and the
reddit/meta clients' equivalent `redactURL` (userinfo/credentials-in-caller-
URL pattern, see `docs/reviews/knowledge-base/credentials-and-untrusted-text.md`).

## Ad group + responsive search ad creation (GA-3b)

`CreateCampaign` extends the GA-2 campaign shell with a real
Campaign→AdGroup→Ad hierarchy, in `adgroup_ad.go`: after the campaign
`:mutate` commits, `createAdGroupAndAd` sends `adGroups:mutate` (a single
`SEARCH_STANDARD` ad group referencing the campaign's resourceName, status
`PAUSED`) then `adGroupAds:mutate` (a `PAUSED` Responsive Search Ad on that
ad group). After the ad is created, if the caller supplied any
`Keywords`/`AudienceSegments`, `createAdGroupTargeting` (GA-4, below) attaches
them to the same ad group before `CreateCampaign` returns — without it, the ad
group carries no targeting and the campaign can never serve, even once
enabled. Both the ad-group/ad steps run unconditionally after every
successful campaign create; there is no way to create a campaign shell with
no children through this path today.

**All ad-group/ad input validation runs BEFORE the first (budget) mutate.**
`CreateCampaign` calls `precomputeAdGroupAdInputs` — which validates the
destination URL (`buildAdFinalURL`), ad copy (`composeAdCopy`), and ad-group
name (`validateEntityName`) with no network calls — immediately after the
budget/campaign name validation and before `campaignBudgets:mutate` is sent.
`createAdGroupAndAd` then takes the precomputed `finalURL`/`headlines`/
`descriptions`/`adGroupName` as arguments and performs no local validation of
its own. This ordering matters: `createAdGroupAndAd` runs LAST, after the
budget and campaign already committed, so validating its inputs only at that
point would let a bad `RegistrationURL` or an over-length ad-group name orphan
a real, already-created budget+campaign for what is purely a local input
error — every other precondition in this file (budget/campaign name length,
attribution fields) is likewise checked before its first upstream call for
the same reason.

**Idempotency** mirrors the budget/campaign convention (create-then-catch,
not Microsoft's find-by-name-first): the ad group has a deterministic
composed name (`composeName("Ad Group", in)`), so a retry with the same
`NameSuffix` collides on Google's `AdGroupError.DUPLICATE_ADGROUP_NAME` and is
reported "already exists, reconcile by name" (`isDuplicateAdGroupNameErr`) —
exactly like the budget/campaign duplicate branches. Unlike those, a
duplicate/ambiguous ad-group outcome is NOT looked up by name to recover its
id, so a retry after that outcome cannot re-attempt the ad create either (no
ad-group id to attach it to) — a documented, intentional limitation requiring
manual reconciliation, consistent with the existing budget/campaign-duplicate
precedent. The ad itself has no unique name (Google permits duplicate ad
content within an ad group) and gets no find-first safety net: a duplicate or
ambiguous ad-group-create always bails out BEFORE the ad-create step runs, so
within a single `CreateCampaign` call — or across retries, which re-hit the
same ad-group duplicate check and bail at the same point — the ad `:mutate`
can never be attempted twice against one ad group.

**AdGroupAd resource names are a composite key**: unlike every other Google
Ads resource (a single trailing numeric id split by `resourceID`), an
AdGroupAd's resourceName trailing segment is `{adGroupId}~{adId}` (tilde-
separated, e.g. `customers/1/adGroupAds/111~222`). `adGroupAdID` splits on
`~` after `resourceID`'s slash-split and requires EXACTLY two components, both
a non-empty run of ASCII digits (`numericID`) — a third tilde-separated
component or a non-numeric half is rejected as malformed (returns `("", "")`)
rather than silently accepted, since the extra/non-numeric text would
otherwise be carried into `res.AdGroupID`/`AdID` and later interpolated into a
`resourceName` path by `UpdateAdGroupAndAdStatus`. `createAdGroupAndAd` also
verifies the returned ad-group-id HALF of the adGroupAd resourceName matches
the ad-group id the ad was actually created under — a mismatch means the
response doesn't describe the ad this call just created, so it is reported
UNCONFIRMED rather than persisted. This composite shape is the single
highest-risk unverified assumption in this slice (no live fixture in-repo to
confirm it against) — if wrong, both `UpdateAdGroupAndAdStatus`'s resourceName
construction and `firstResourceName`'s extraction on ad create would silently
misbehave.

**Ad copy** (`composeAdCopy`) accepts caller-supplied `Headlines`/
`Descriptions`, trims/rune-truncates/dedupes them (`boundedUniqueCopy`), then
pads with deterministic eventName/project-derived placeholders
(`defaultHeadlines`/`defaultDescriptions` via `padUnique`) up to Google's v23
RSA minimums (3 headlines ≤30, 2 descriptions ≤90). Caller-supplied entries are
accepted up to the maximum (15 headlines, 4 descriptions), with later entries
beyond those limits silently dropped. A caller with zero usable copy AND an
empty EventName is a hard error (there is nothing to advertise).

Those limits are WEIGHTED character counts, not plain rune counts:
`googleAdsCharWeight` scores CJK/full-width runes (Hangul Jamo and Syllables,
CJK Radicals through Yi, CJK Compatibility Ideographs and Forms, Fullwidth
Forms and Signs, CJK Ext. B-G) as 2 and everything else as 1, and
`truncateWeighted` cuts to that budget on a rune boundary. All-wide-character
copy therefore fits 15 headline / 45 description characters. This matches the
Microsoft client's equivalent rule rather than differing from it — an earlier
version of this file claimed there was no double-width halving here, which
contradicted `ad_copy.go` and is corrected.

**The ad's final URL** (`buildAdFinalURL`) is the brief's `RegistrationURL`
tagged with `utm_source=google`, `utm_medium=cpc`, `utm_campaign` (from
EventSlug, falling back to EventName then NameSuffix), and `utm_content`
(from Project). `utm_source` and `utm_medium` are set UNCONDITIONALLY
(`q.Set`), overwriting any values the registration URL carried — a click on
a Google CPC ad must be attributed to Google/CPC, never to an earlier
channel's tag. `utm_campaign` and `utm_content` use `setIfAbsent`, so a
caller-supplied value for either wins. Every other query param the URL
already carries is preserved untouched. An empty/non-http(s)/no-host registration URL is rejected
before any mutate runs, so a bad input never orphans an ad group with no ad.

`Client.UpdateAdGroupAndAdStatus` sends the ad group status update then the ad
status update, stopping on the first failure without attempting the second —
the caller owns the all-or-nothing cascade semantics, not this method.

## Status toggling (GA-3c)

`GoogleAdsDispatcher.ToggleStatus` (`internal/dispatch/googleads.go`) implements
PAUSE cascading for the campaign-level pause that GA-2 introduced down to the ad group +
ad GA-3b creates, mirroring the reddit adapter's child-cascade contract:

- **PAUSE flips the campaign first**, then the ad group/ad. Pausing the
  parent stops delivery immediately regardless of whether the child update
  that follows succeeds — if that child update fails or is UNCONFIRMED, the
  error still surfaces to the caller (the campaign is left paused, the child
  status is unresolved) rather than being swallowed. If either child id is
  absent (e.g. a campaign shell with no fully-created ad group/ad — see
  GA-3b's duplicate/ambiguous-outcome limitations), there is nothing to pause
  downstream and only the campaign is toggled.

- **ACTIVATE is refused** with `domain.ErrCampaignNotProvisioned` (mapped to a 409 without
  calling Google) unless the ad group/ad are fully provisioned AND GA-4's targeting step
  persisted at least one keyword criterion (audience criteria alone are observation-only and
  don't qualify — see "Keyword + audience targeting (GA-4)" below). A campaign without keyword
  targeting cannot deliver, so enabling it would report false success. When the guard passes,
  ACTIVATE cascades children-first (children activated before campaign) so a campaign never
  reports ENABLED before its ad group/ad already do.

- **Both directions are refused** with `domain.ErrCampaignAccountMismatch` (409, Google never
  contacted) when the campaign was created under a different customer than the project's
  connection currently resolves to. `googleAdsCreationCustomerID` recovers the creation-time
  customer id from the persisted `Result` blob — its `customerId` field, falling back to the
  customer segment of the stored `googleAdsUrl` for campaigns written before that field existed —
  and compares it with `client.CustomerID()`; the
  check sits ABOVE the PAUSE/ACTIVATE branch deliberately, because it is not about which way the
  status is moving. The stored campaign/ad-group/ad ids are bare numerics, unique only within the
  customer they were created under, and `UpdateGoogleAds` can re-point a connection between create
  and toggle — so on an id collision the mutate would succeed against ANOTHER account's resources,
  pausing or enabling something this project does not own. `ReadMetrics` enforces the same
  invariant for the same reason (see "Metrics reads (GA-5)"); it matters more here because this
  path mutates. A campaign whose blob carries no creation customer id (created before the id was
  recorded) skips the check rather than failing closed.

`googleAdsChildIDs` recovers the ad-group/ad ids from the campaign's
persisted `Result` blob (the JSON GA-3b's create path stores) — a
missing/unparseable blob yields empty ids, which is what drives the "nothing
to cascade to" branches above.

## Keyword + audience targeting (GA-4)

`createAdGroupTargeting` (in `targeting.go`) attaches positive Search
keywords and/or existing audience segments to the ad group `createAdGroupAndAd`
just built, as a SINGLE `adGroupCriteria:mutate` call carrying one operation
per criterion — batched (not one call per criterion) so the whole set shares
one atomic outcome, the same "no partial state" reasoning as every other
mutate here, just extended to N operations. Input is validated up front
(`validateKeywords`/`validateAudienceSegments`), before any mutate call in
`createAdGroupAndAd` runs, alongside the existing URL/copy/name checks — a bad
keyword or audience value must not leave a created ad group + ad with a
failed targeting step the caller has to puzzle out separately.

**Keywords** (`Keyword{Text, MatchType}`) are positive Search criteria only;
`MatchType` is one of `EXACT`/`PHRASE`/`BROAD`. `Text` is capped at v23's
80-rune `KeywordInfo.text` limit. Up to 60 keywords per call (a sanity cap on
this broker's input, not a Google Ads platform limit; raised from 20 in
LFXV2-3259, which the brief generator's ~38-keyword output exceeded); duplicates (same
matchType+text) are deduped rather than rejected.

**Audience segments** are EXISTING Google Ads audience resource names — this
client does not create audiences. For SEARCH campaigns, only `.../userLists/{id}`
(Customer Match / remarketing list) resource names are accepted; any other shape
(`customAudiences`, `userInterest`, `combinedAudience`, `detailedDemographic`, …)
is rejected — deliberately narrow scope limiting to externally built user lists
that have already been provisioned outside this service. Also capped at 20 per
call, deduped by resource name.

Because `userList` is the only shape that survives validation, the
`adGroupCriterionCreate` payload carries no `customAudience` field and
`createAdGroupTargeting` does not branch on the criterion kind: a
customAudience arm there would be unreachable, and a `switch` over an
unrecognized kind silently emits a criterion with NO oneof set — a 4xx arriving
only after the budget, campaign, ad group and ad already exist. The one place
`customAudiences` is still recognized is `audienceCriterionField`, and only so
that `validateAudienceSegments` can reject it with its actual reason ("not
supported for SEARCH campaigns") instead of the generic
unrecognized-resource-name error, which would send a caller hunting for a typo
in a perfectly well-formed name.

These resource names are Google Ads' own, arriving through this dispatcher's
configuration. They are NOT the `campaign_audiences` resource in
`docs/api-catalog.md`: that resource's `platform` enum is `hubspot` only
(`design/audience.go`), so it holds HubSpot master-list pointers, which can
never appear as a Google Ads criterion.

**Observation vs targeting — the audience-restriction gotcha**: a Search ad
group's audience criteria default to TARGETING (restrictive — narrows
delivery to ONLY that audience) unless a `targetingSetting.targetRestrictions`
with `targetingDimension: "AUDIENCE", bidOnly: true` is declared at the SAME
level the criteria are attached. GA-4's audience criteria are created as
`AdGroupCriterion`s, so this must be set on the AD GROUP `:mutate` create, not
the campaign create — Google requires `targetingSetting` live at the same
level as the criterion, and even rejects setting it on an `AdGroup` while the
parent `Campaign` has one (per Google's `UpdateAudienceTargetRestriction`
sample, which reads/writes `ad_group.targeting_setting`, not
`campaign.targeting_setting`). `createAdGroupAndAd` (`adgroup_ad.go`) sets it
on the ad group create WHENEVER the validated `audienceSegments` list is
non-empty (and omits it entirely otherwise) — so an audience segment added for
bid/reporting purposes doesn't silently narrow delivery to that segment alone.
(An earlier version of this code set it on the campaign create instead, which
Google's docs confirm has no effect on ad-group-level criteria.)

Every criterion is created `ENABLED` (not `PAUSED` like the ad group/ad
shell): a criterion's own status is one more gate on top of its ancestors (ad
group, ad, campaign) already being enabled — Google won't serve it while any
ancestor is PAUSED, so creating it ENABLED now means the campaign is
immediately serve-ready the moment a human flips the ad group/ad/campaign to
ENABLED, with no separate targeting-activation step.

**AdGroupCriterion resource names share AdGroupAd's composite shape**
(`{adGroupId}~{criterionId}`) — `compositeResourceID` (factored out of GA-3's
`adGroupAdID` in `adgroup_ad.go`) is reused here to extract the criterion id.

**Duplicate-criterion classification is unverified** for this resource
(unlike the budget/campaign/ad-group `DUPLICATE_NAME` family): any 4xx on
`adGroupCriteria:mutate` — including a possible duplicate on retry — is
reported as a straightforward failure, not reconciled by a duplicate
predicate. An ambiguous outcome (5xx/429/transport, or a 2xx with a
malformed/short mutate response) is reported UNCONFIRMED, same convention as
GA-2/GA-3.

`CampaignResult.KeywordCriteriaIDs`/`AudienceCriteriaIDs` are populated from
the mutate response only when targeting was attempted and succeeded; both are
empty if targeting was never attempted (no `Keywords`/`AudienceSegments`
supplied) or failed before any criterion resource name could be parsed. The
per-platform dispatcher config (`internal/dispatch/googleads.go`,
`googleAdsConfig.Keywords`/`.AudienceSegments`) maps the wire JSON shape 1:1
into `CampaignInput.Keywords`/`.AudienceSegments`.

## Campaign settings readback (LFXV2-3067)

`GetCampaignSettings` (in `campaign_settings.go`) reads a campaign's CURRENT
configuration — budget amount, budget period, delivery method, sharing, channel type,
bidding strategy, name, status and flight date-times — via a single GAQL
`googleAds:search`. It is the read metrics cannot be: impressions, clicks, cost and CTR
do not describe a campaign's *configuration*, so no reading of them can show that the
budget upstream is not the budget the campaign row records.

**Every field on `CampaignSettings` is a POINTER.** A setting Google did not return is
ABSENT (nil), never a zero value. This is the type's central decision: a `0` standing in
for an unread budget is indistinguishable from a campaign with a genuinely zero budget,
and the two mean opposite things to an operator. The readback is partial in a way a
metrics read is not — `campaign_budget` is a separate resource joined onto the campaign,
so its fields can be missing while the campaign's own fields are present.

**The field names are version-scoped to v23, and two are not the request-side spellings:**

* `campaign.start_date_time` / `campaign.end_date_time` — in v23 these REPLACED
  `campaign.start_date` / `campaign.end_date`, which are rejected as unrecognized. Format
  is `yyyy-MM-dd HH:mm:ss` in the ad account's timezone, so they are carried verbatim and
  never parsed here: a timezone this client does not know makes any computed instant a
  guess. The pre-v23 `2037-12-30` no-end-date sentinel is gone — no end date is an absent
  field.
* `campaign_budget.amount_micros` and `campaign_budget.total_amount_micros` are MUTUALLY
  EXCLUSIVE — the first for a `DAILY` budget, the second for `CUSTOM_PERIOD`. Reading only
  the first reports a lifetime-budget campaign as having no budget at all.
* `campaign_budget.period` is `DAILY` or `CUSTOM_PERIOD`; there is no `LIFETIME` value.
  The translation into `model.BudgetType` lives in the dispatcher, not here.

**Two budget guards, not one.** A row is refused when it carries BOTH amounts, and
separately when the amount it carries CONTRADICTS the period: `DAILY` beside a
`total_amount_micros`, or `CUSTOM_PERIOD` beside an `amount_micros`. The second is a
RESPONSE-INTEGRITY check about the row contradicting ITSELF — the two identity halves of the
budget disagree, so no reading of it can be trusted, and there is no way to tell which half is
the wrong one. It is not a stand-in for careful field selection downstream: since LFXV2-3067
`googleAdsUpstreamBudgetAmount` selects the amount THROUGH `googleAdsBudgetTypeFromPeriod`
rather than by presence, so the two are independent. A malformed row is still refused here
rather than passed on, because reporting a budget from it — by either field — would attribute
to the campaign a number the platform's own invariant says cannot describe it.

The period is `strings.TrimSpace`d for THIS COMPARISON ONLY (LFXV2-3067). `blankToNil` keeps
a non-blank value VERBATIM, so a padded `" DAILY "` matched neither arm of the exact-equality
switch and slipped past BOTH refusals — the precise pair the second guard exists to catch,
leaving an operator chasing a phantom budget divergence. Trimming is correct HERE and wrong
elsewhere, and the distinction is the value's KIND, not the operation: `period` is a
closed-set ENUM, so recognising `" DAILY "` as `DAILY` DISCOVERS what the value already
unambiguously is, and the trimmed string only CHOOSES A REFUSAL — it is never written back
onto `settings` and never populates a compared field. `blankToNil`'s warning is about the
opposite case, where trimming an opaque IDENTIFIER or a strictly-parsed date INVENTS a
well-formed value the platform never sent, manufacturing agreement instead of detecting
contradiction. That is also why `googleAdsBudgetTypeFromPeriod` still does NOT trim: it
produces a COMPARED value and now also SELECTS the budget amount, so a padded period must keep
yielding an `unknown` verdict on both.

An ABSENT period PASSES both guards, and so do `UNKNOWN`/`UNSPECIFIED`. Absence already
means "Google did not report this field" on every `CampaignSettings` pointer — a partial
read is the ordinary case — so it cannot also start meaning "inconsistent pair" without
breaking that meaning. It is SAFE downstream too, and that half is ENFORCED rather than
assumed: `googleAdsUpstreamBudgetAmount` selects the amount through
`googleAdsBudgetTypeFromPeriod`, so a period naming neither real value selects no amount and
the field reads `unknown` rather than comparing a quantity of unestablished meaning. A value
Google explicitly declined to name contradicts nothing, and selects nothing either. See
`docs/knowledge/log/2026-08-24-LFXV2-3067-the-period-decides-which-amount.md`.

`campaign_budget` is an ATTRIBUTED resource of `campaign`, which is what allows its fields
in a `FROM campaign` query. Attribution does not segment, so the at-most-one-row guard
still holds — and the query deliberately selects no `metrics.*` or `segments.*` field,
either of which would segment the result and make that guard fire on healthy campaigns.

Same fail-closed contract as `GetCampaign`: one campaign yields settings, a genuine
absence yields `(nil, nil)`, and anything unverifiable is an error. The same
decode-integrity guards run too (invalid UTF-8, unpaired surrogate escapes, duplicate JSON
keys, an unhonoured id filter). Unlike `GetCampaign`, REMOVED campaigns are NOT filtered:
"removed upstream" is the most actionable divergence this read can surface, and excluding
it would report the campaign as absent and hide the finding.

A budget that is PRESENT but unparseable is an error rather than an absence —
`parseSettingsInt` differs from `parseMetricInt` in exactly this way, because a budget is
not a counter: an empty one is a field that could not be read, and answering `0` would be
a claim about the campaign rather than about the read. The malformed value is never echoed
into the error, only the field name.

## Metrics reads (GA-5)

`GetCampaignMetrics` (in `metrics.go`) reads live impressions, clicks, cost,
CTR and CONVERSIONS for one campaign over a predefined date-range window (e.g.
`LAST_30_DAYS`) via a single GAQL `googleAds:search` query.

`metrics.conversions` is handled differently from the other metrics in two ways,
both of which are properties of the upstream encoding. It is declared **DOUBLE**
in Google's field reference — Google credits fractional conversions under
data-driven and position-based attribution — so it arrives as a BARE JSON NUMBER
rather than one of the quoted int64 strings, and its fraction is carried through
UNROUNDED: a campaign holding 0.4 of a conversion that was rounded to 0 would be
reported as converting nobody, and the `no_conversions` rule reads exactly this
number. Second, because the field is ALWAYS in the SELECT list and Google Ads
REST is proto3 JSON (which omits default-valued fields), an **absent conversions
member is the encoding of a measured 0.0**, not "unmeasured" — the adapter
materialises a non-nil zero, the same way `parseMetricInt` treats an omitted
impressions value as a measured 0. A nil conversions count is reserved for the
platforms that cannot report one at all (Meta, X, Reddit, email), which is what
the pointer in `model.CampaignMetrics` exists to distinguish. The window and campaign id are
both validated as constrained (an allow-list of GAQL predefined-date-range
literals, and digits-only respectively) BEFORE string concatenation into the
query, since GAQL has no parameterized queries. int64 metric fields arrive as
JSON strings in the v23 REST response and are parsed via `parseMetricInt`, which
treats empty strings (Google Ads omits zero-valued optional metrics) as zeros
rather than parse errors. CTR is computed client-side (Clicks/Impressions, 0
when Impressions is 0 — never divides by zero). The return type `CampaignMetrics`
is distinct from the domain type `model.CampaignMetrics` (an application-level
neutral staging area), converted at the dispatcher boundary.

`WindowFor` maps the platform-agnostic `model.MetricsWindow` (`last_30_days`, …)
onto this package's GAQL literals. It lives HERE, not in the dispatcher, so
Google's dialect never escapes the platform package — the API surface and the
`MetricsReader` interface both speak only the neutral vocabulary. It is a
translation, not a security boundary: the mapped literal still goes through the
`validMetricsWindows` allow-list in `GetCampaignMetrics`, which is what actually
guards the GAQL concatenation. A window it cannot map yields
`ErrUnsupportedWindow`, which the dispatcher joins with
`domain.ErrMetricsWindowUnsupported` so the service layer answers 400 (caller
input) rather than 503, without contacting Google Ads at all. Every branch of that
mapping is pinned by `TestWindowFor_CoversEveryModelWindow` against the literal GAQL
strings, and the table is size-checked against `validMetricsWindows`: a wrong literal
would compile, pass the allow-list, and silently report the WRONG REPORTING PERIOD,
whose only symptom is plausible numbers for the wrong dates.

**A decode error names the failing field, never its value.** Metric values arrive as
strings in the upstream body, and the service's default failure branch renders a
platform error into a warning log — so interpolating the raw value into the error text
is a log-injection path from an attacker-influenceable response. `GetCampaignMetrics`
reports which of `impressions` / `clicks` / `costMicros` failed to parse and stops
there; the value stays out of the log stream. Pinned by
`TestGetCampaignMetrics_NonNumericMetricFieldIsTransportError`, which asserts both
halves — the field name present, the value absent.

`Client.CustomerID` exposes the ad account the client is bound to, and
`CampaignResult.CustomerID` records the one a campaign was CREATED under (stamped on
every partial too, so an ambiguous create is reconcilable in the right account). The
pair exists because a Google Ads campaign id is unique only WITHIN a customer, while
the project's connection is mutable — `UpdateGoogleAds` can re-point it at another
account. `ReadMetrics` compares the two before querying: mismatched, it returns
`domain.ErrCampaignAccountMismatch` (409) rather than issuing a GAQL query that
Google answers with an empty result set indistinguishable from genuinely zero
activity — or, on an id collision, with another account's numbers. Campaign rows
predating the field fall back to the `ocid` parameter of the stored
`googleAdsUrl`; when neither is present the identity is unknown, which cannot prove
a mismatch, so the read proceeds.

**UNVERIFIED ASSUMPTION**: a `segments.date` WHERE-filter without `segments.date`
in SELECT returns one row aggregated over the whole window, not one row per day.
Not yet verified against a live Google Ads account with >1 day of data in the
window.

## Geo targeting (LFXV2-3283)

`geo.go` owns location targeting for BOTH channels. Before it, neither create
path attached location criteria: `CampaignInput` carried no geo field at all, so
a campaign created through the service served wherever the ACCOUNT's defaults
allowed — for an event campaign, most of the budget spent outside the region it
was bought for.

**Country codes in, Google constants out.** Callers pass ISO 3166-1 alpha-2
codes (`CampaignInput.GeoTargets`), the vocabulary `metaConfig`/`redditConfig`
already use. Google Ads does not address locations that way: a location
criterion carries `geoTargetConstants/{id}`, a NUMERIC constant from Google's
published table. `geoTargetConstants` maps between them, ported verbatim from the
legacy Express implementation's `GEO_TARGET_MAP` (`lfx-self-serve`
`campaign-proxy.service.ts`) so both paths target the same places during the
cutover. The ids bear NO arithmetic relation to the code (`US`→2840,
`GB`→2826), so a mis-transcribed entry targets the WRONG COUNTRY while looking
entirely valid — which is why the map is a curated country subset that is
reviewed, rather than a fetched data file.

**The attach LEVEL differs per channel, and that is the trap.** Search takes
CAMPAIGN-level criteria (`campaignCriteria:mutate`); Demand Gen REJECTS those and
takes the same criterion on the AD GROUP (`adGroupCriteria:mutate`). A single
implementation attaching at the campaign level works on Search and is refused on
Demand Gen — after the budget and campaign have already been created and cost
money. Hence two payload types and two functions named for their level
(`createCampaignGeoTargeting` / `createAdGroupGeoTargeting`) rather than one with
a level parameter that a caller could get wrong.

**An unmapped code is REFUSED, not dropped.** `validateGeoTargets` runs in the
PREFLIGHT, before the first (budget) mutate, so a typo like `"USA"` fails while
nothing paid exists. Dropping it instead would create a campaign with no criteria
that spends worldwide and reports success — the exact defect this slice fixes.
This is the opposite of meta's default-to-`US`, which is safe there only because
Meta's criteria attach during creation rather than after it. It also runs in
`ValidateCampaignInput`, so the ADOPTION path (which returns before
`CreateCampaign`) cannot accept an input the create path would refuse.

**Empty stays a no-op.** No geo targets means no criteria request at all and the
pre-LFXV2-3283 behaviour, because every caller predating the field omits it and
failing them outright would break dispatches that work today. The dispatcher logs
a WARN instead, so an untargeted create is findable in the logs rather than
inferred from a Google Ads bill. `CampaignResult.GeoCriterionIDs` records what
was created — campaignCriterion ids on Search, adGroupCriterion ids on Demand
Gen, so reconcile against the level the channel uses.

Failures follow the same contract as every other post-campaign step: the criteria
call happens AFTER the campaign exists, so an error is returned ALONGSIDE the
non-nil result, never as `(nil, err)` that would discard the claim on a campaign
that spends.

## Scope

GA-1 is the scaffold (auth + request layer + GAQL search); GA-2 is campaign
creation (`:mutate`); GA-3a is ad-copy generation and final-URL building
(`ad_copy.go`, this file's previous section); GA-3b is the ad-group/ad
creation cascade that consumes it; GA-3c is the dispatcher-level
status-toggle cascade over that ad group/ad; GA-4 is keyword/audience-segment
targeting on that same ad group; GA-5 is metrics reads (`metrics.go`, see
above); geo/location targeting (`geo.go`, LFXV2-3283) spans both channels and
attaches at a different level in each (see "Geo targeting" above). The orchestrator dispatcher (registering
`google-ads` so briefs dispatch upstream) is wired in
`internal/dispatch/googleads.go` (LFXV2-2636). Keyword actions
(pause/adjust an individual keyword post-creation) follow in later GA
slices.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` googleads adapter (see [internal/dispatch](internal-dispatch.md))
interprets an OAuth2 application (clientId/secret + refreshToken) PLUS a Google Ads API
developer token; AccountConfig comes from AccountID (the customer id) + an OPTIONAL
`login_customer_id` (the manager/MCC account, from the connection's ProviderConfig).
Budget (`googleAdsConfig.budget`) is in the ACCOUNT's currency (no FX). The client today
creates a PAUSED search-campaign shell (budget → campaign); its two-step hierarchy means a
PRE-attachment (budget-stage) orphan is reconciled by its deterministic
`CampaignBudgetName`, but once the campaign attaches a non-shared budget's name
synchronizes to the campaign name, so a campaign-stage partial reconciles the budget by
`CampaignBudgetID` instead (the partial carries both). Either way the dispatcher returns a
non-nil result (retaining the claim) on an ambiguous/duplicate-name create rather than
releasing on an empty id.

It implements PAUSE/ACTIVATE cascading — see "Status toggling (GA-3c)" above for the
cascade order and the ACTIVATE provisioning gate. Note the vocabulary: Google spells the
serving state **ENABLED**, not ACTIVE.

Microsoft Ads has a creation dispatcher; its status-TOGGLE capability lands separately.

## Account discovery

`Client.ListAccessibleCustomers` enumerates the ad accounts reachable with the credential,
returning `AccessibleCustomer{ResourceName, DescriptiveName}`. Two things make it unlike every
other call in this package.

**It runs with no `CustomerID`.** `doRequest` validates `c.account.CustomerID` as digits-only
before building any request, but this call is how a caller LEARNS a customer id — a connection
is authorized first and an account chosen afterwards, from this very list. So the
account-agnostic paths call `doRequestValidated`, which is `doRequest` with the id precondition
discharged by the caller. It exists only so those paths still share one copy of the URL
construction, header set, body bounding, retry gating, and `apiError`/`transportError`
classification; `validateLoginCustomerID` still runs, because the `login-customer-id` header is
still attached and still has to be well-formed. `gaqlSearchForCustomer` is the same idea for
searches: it takes an EXPLICIT customer id (validated there, since it is interpolated into the
resource path) rather than the client's configured one, and `gaqlSearch` is now a thin
delegation to it.

**A manager credential needs the hierarchy walked, and the flat list is not consulted at all.**
`customers:listAccessibleCustomers` returns the accounts the authenticated user can act on
DIRECTLY; a `login-customer-id` header does not make it enumerate that manager's children —
that is a property of the endpoint, not of the header. On an MCC connection the flat list is
therefore often just the manager itself, with every child ad account missing.

So `ListAccessibleCustomers` has two modes, they do not merge, and **each consults exactly one
data source**. That is not the same as one HTTP request: the manager mode's GAQL read pages
until `nextPageToken` is empty, and either mode retries a 429. What the invariant rules out is
a mode issuing a request whose response it will not read. The mode is decided from
`login_customer_id` before anything goes over the wire:

- **No `login_customer_id`.** The direct list IS the answer. Every account in it is one the
  credential addresses on its own behalf. A manager account can still be in there and cannot
  hold campaigns, but the response carries no `manager` flag, so there is nothing to recognise
  it by; a round-trip per row to find out would cost more than it saves on a list this short.
- **`login_customer_id` set.** `expandManagerHierarchy` returns immediately, and the flat endpoint
  is **never called**. The selectable set is exactly `listManagerClients`' output: a
  `customer_client` GAQL query scoped to the manager, requesting only `status = 'ENABLED'`
  clients and dropping rows where `customer_client.manager` is true.

Not calling it is the point, not an oversight. `listAccessibleCustomers` is UNSCOPED — the
header does not filter it — while every OTHER request this client makes carries that header, so
an account is addressable through this client only if it sits under the configured manager. An
account the user reaches directly but which belongs to a different hierarchy comes back in the
flat list and then fails `PERMISSION_DENIED` the moment anything is done with it. Merging it in
offers the caller a choice that cannot work, and the failure lands at first dispatch — long
after the connection was saved — where it reads as a credential problem rather than a
wrong-account one. Nothing addressable is lost by dropping it: an account under the manager is
in the expansion too.

The expansion is also the only source of `descriptive_name` — the flat endpoint returns resource
names alone — so accounts come back labelled in manager mode and unlabelled without one.
Expansion rows are deduplicated by resource name, since `customer_client` reports a client once
per path through the hierarchy and a client of a sub-manager that is itself a client of the root
appears twice. A row with no id is a hard error rather than a silent drop, since dropping it
would understate the list the operator chooses from.

Flat-list resource names are validated as `customers/{digits}` in direct mode, where a caller
persists the value as the connection's account id and interpolates it into later request
paths. That validation used to run in manager mode too, on rows nothing would consume — which
only meant the discarded response had one more way to fail the request. Manager-mode ids are
validated inside `listManagerClients` instead.

**Only one data source means only one failure mode.** Fetching the flat list and then throwing
it away spent request quota and whatever deadline the caller passed down, but the behavioural
cost was worse: a timeout, 429, or 5xx on a response that was never going to be read still
propagated out and failed the whole discovery, even though the hierarchy query alone would
have answered correctly. `TestListAccessibleCustomers_ManagerModeNeverCallsTheFlatList` pins
this by failing the flat endpoint with a 500 and requiring discovery to succeed anyway —
every other manager-mode test serves both endpoints and so passes either way.

The REST binding is GET (not the POST used by `:search` and `:mutate`), it takes no request
body at all, and it is sent with `idempotent=true` — a pure read, so retrying a 429 cannot
double-apply anything.


## Keyword and audience insights, and keyword actions (LFXV2-2641)

`keywords.go` adds two GAQL reads and one mutation. None of them creates a keyword — criterion
creation remains GA-4's `createAdGroupTargeting`.

**The reads are scoped to the caller's own CAMPAIGNS, not to the connected account, and that
distinction is the authorization boundary.** An earlier version scoped them only to the client's
customer id, on the reasoning that "the connection IS the scope". That reasoning does not hold
here: Google Ads is ONE customer shared across every foundation (see `docs/architecture.md`,
"Account Tenancy"), so a connection-scoped query returns every project's keyword text, campaign
ids, spend and demographic distribution to any `campaign_manager`. Both reads now take the
`platform_campaign_id`s the service holds for the project — read with `project_id` in the SQL
WHERE clause, never filtered in Go after an unscoped read — and render them into a
`campaign.id IN (...)` predicate via `campaignScopePredicate`.

`campaignScopePredicate` REFUSES an empty list rather than returning an empty string, because an
empty predicate concatenated into the query silently restores the account-wide read — and it does
so for the input most likely to occur, a project that has dispatched nothing. The orchestrator
answers that case earlier still, returning an empty result WITHOUT issuing any upstream query;
the adapter's refusal is defence in depth for a future caller in another package. Ids are
validated digits-only for the same reason `GetCampaignMetrics` validates its campaign id: GAQL
has no parameterized queries and these values reach the string by concatenation.

The consequence worth stating plainly is a BEHAVIOUR CHANGE: a campaign created outside this
service, or claimed but not yet dispatched, has no dispatched row and is therefore not in scope,
so its keywords and demographics no longer appear.

**Sending the filter is only half of the boundary; the response has to be re-checked against
it.** `campaignScopePredicate` returns the canonical scope SET alongside the predicate string,
and both scoped reads pass every response row through `assertCampaignInScope` before using it.
The predicate is the filter REQUESTED; the set is the filter ENFORCED. On a customer shared
across every foundation, a response carrying a campaign outside the requested set means the
WHERE clause was not honoured, and the rows on offer are another project's keywords, spend and
demographics — so the check errors on the whole response rather than skipping the row. Skipping
would reduce an unhonoured query to "this project has little data", which is the clean partial
answer a caller totals and acts on. This is the rule `campaign_lookup.go` already applies to its
own id filter, and the two surfaces are deliberately written the same way.

Presence is NOT membership, and that distinction is the whole finding. An earlier revision
guarded `campaign.id` only for being non-empty, on the reasoning that an absent id means the
SELECT and the decode struct have drifted apart. That reasoning is sound and still holds, but it
answers "does this row name a campaign", not "does it name one of the campaigns asked for" — so
a row for campaign 999 returned against a request scoped to 555 was admitted as a successful
read. The set membership check subsumes the presence check rather than replacing it: an empty
id does not canonicalise and fails the same guard.

**The truncation PROBE row is a returned row, so it is checked like every other one.**
`GetKeywordPerformance` asks for `maxKeywordRows+1` and reports the extra row as `Truncated`.
A first revision discarded the probe with `rows = rows[:maxKeywordRows]` BEFORE the loop that
calls `assertCampaignInScope`, which exempted the one row most worth checking: a response whose
51st row named an out-of-scope campaign returned a clean, capped answer with `truncated: true`
and no error. "Every response row is re-checked" was therefore true of 50 rows out of 51, and
the hole sat precisely where a caller reads the result as "this project has more keywords than
we can show" — a claim sourced from a row proving the filter was not honoured. The cap now
governs the APPEND, not the validation: all `maxKeywordRows+1` rows are decoded and
tenant-checked, and rows at index `>= maxKeywordRows` are skipped only when building the output
slice. A cap is a presentation concern; a tenant check is not, and the two must not share a
control-flow decision.

The ids are canonicalised on BOTH sides before comparison (`canonicalCampaignID`), so the
comparison compares campaigns rather than text — otherwise `0555` would read as a foreign
campaign, and the boundary would depend on how the upstream chose to spell a number.

`GetAudienceInsights` selects `campaign.id` even though no bucket reports it, purely to make
this check possible: without that column the response carries no evidence of which campaign a
bucket's impressions and spend came from, so an unhonoured filter is indistinguishable from a
correct answer. Its check runs BEFORE the row's metrics are aggregated — once a foreign row's
impressions are summed into a bucket they cannot be told from the project's own. Note that a
stub server answers whatever fixture a test wrote regardless of the SELECT, so the projection
needs a test of its own; asserting `campaign.id` appears anywhere in the query text matches the
WHERE clause and proves nothing about the SELECT.

**The narrowing has to be swept across every surface that DESCRIBES these reads, not only the
one that performs them.** The scope predicate landed with the type godocs, method godocs, test
rationales, the API catalog row and this file still calling the results "account-wide", "the
account's keywords" or "the whole account's spend" — a published boundary WIDER than the one
the code enforces. That is not a cosmetic lag. A consumer reading `AudienceInsights` as the
account's audience presents another foundation's demographics under this project's name; a
future author reading `campaignIDs` as a filter rather than as the tenant boundary drops it as
an optimisation; and a test whose stated reason is "an ACCOUNT-WIDE read returns keywords this
service never created" rests on a premise that is now false, even where its conclusion survives
(adopted campaigns and UI edits reach the same place). The rule is that whoever narrows a read
adopts every claim about it they leave standing.

The distinction to preserve while sweeping: a comment saying these reads WOULD be account-wide
without the predicate is TRUE and load-bearing — `campaignScopePredicate`'s own refusal, the
orchestrator's empty-scope early return and the dispatcher's post-filter guard are all written
that way on purpose. What must go is any claim that the RESULT is account-wide.

**The keyword read is capped, and says so.** `GetKeywordPerformance` orders by impressions
descending and asks for `maxKeywordRows+1` rows; the extra row is dropped and reported as
`Truncated`. That probe is the whole mechanism: without it a caller receiving exactly the cap
cannot distinguish a small result set from a truncated large one, and would total the slice as
the project's whole spend. The status predicate is an ALLOW-LIST (`ENABLED`, `PAUSED`), not an exclusion of `REMOVED`:
`AdGroupCriterionStatus` also carries `UNSPECIFIED` and `UNKNOWN`, and an omitted proto field
decodes to `""` — all three survive `!= 'REMOVED'` and would be offered as actionable rows
carrying a pause/remove that cannot apply. `status` and `match_type` are additionally
NORMALISED onto the closed vocabularies the API contract declares. That is not cosmetic: Goa
emits response validation in the generated CLIENT, not the server, so one out-of-enum value
makes a client reject the ENTIRE response. Unrecognised values fold onto `UNKNOWN` rather than
dropping the row, following this package's existing reading — a caller must be able to tell
"Google said something we don't handle" from "Google said nothing". A row missing
its criterion or ad-group id is a hard error rather than a returned row, because the
keyword-actions endpoint needs BOTH ids to address a criterion.

**Only POSITIVE keywords are published, and POLARITY is not TYPE.** `keyword_view` being
type-scoped is what makes a criterion-type predicate unnecessary, and that was mistaken for
"everything the view returns is actionable". It is not: the view carries BOTH polarities, so
NEGATIVE keywords come back through the same read. The read now selects
`ad_group_criterion.negative`, restricts the query with `negative = FALSE`, AND re-checks the
field before publishing a row — the same requested/ENFORCED split `assertCampaignInScope` draws,
because a WHERE clause is what was asked for, not what was honoured. **Absence means POSITIVE**:
`negative` is a proto bool, protobuf JSON omits it when false, so the ordinary keyword arrives
with the key missing; reading absence as "unknown" would empty the endpoint, the same defect
that shipped once on the mutate path. A negative row is DROPPED rather than failing the whole
response — unlike a foreign campaign, which proves the campaign filter was not honoured and
invalidates every row. It matters because each row is published as the `criterion_id` +
`ad_group_id` handle `keyword-actions` takes, and that endpoint REFUSES a negative criterion:
publishing one offers a handle whose only advertised use cannot succeed, and it consumes a
capped slot, making `Truncated` describe a set containing unactionable rows.

**Audience is three queries, and partial success is not an outcome.** Age and gender are
criterion views (`age_range_view`, `gender_view`); device is a SEGMENT of the `campaign`
resource, which is why it selects `segments.device FROM campaign` while the others select a
criterion field from a `*_view`. Because the device query segments campaigns, it returns one
row per (campaign, device) pair, so buckets are AGGREGATED by value rather than assumed unique
— taking the last row per device would report a single campaign's numbers as the whole
SCOPE's, i.e. as every campaign the project owns. (Never as the ACCOUNT's: both reads in this
file are campaign-scoped, which is what `campaignScopePredicate` is for.)
CTR is computed after aggregation, never averaged per row. A failure in any one breakdown
fails the whole read: each dimension independently covers the same traffic, and a caller shown
two of three cannot tell the third is missing rather than empty. Google's
`UNDETERMINED`/`UNKNOWN` buckets are returned as-is, since they are real unattributed traffic
and dropping them makes the buckets silently under-sum.

**Keyword actions are atomic and can only reduce delivery.** `PAUSE` and `REMOVE` are the only
supported actions; there is deliberately no `ENABLE`, because re-enabling a keyword restarts
spend and this surface exists to reduce what serves. `partialFailure` is never set, so one
rejected operation rolls the whole batch back — a caller pausing eight keywords to stop a
budget leak is never told that five were paused and left to work out which three still spend.
`PAUSE` is an update carrying `updateMask: "status"`; without that mask Google ignores the
field and the keyword keeps serving while the call reports success. `REMOVE` is a `remove`
operation, and it is IRREVERSIBLE upstream — a removed criterion cannot be re-enabled, only
re-created with a new id.

`ValidateKeywordActions` is split out from `ApplyKeywordActions` so the batch can be rejected
before any credential work happens. Both ids must be digits-only, for the same reason
`customerIDRE` guards the metrics query: they are concatenated into a resource name. A batch
naming the same criterion twice is REFUSED rather than de-duplicated — unlike the create
path's dedupe, two entries can carry different actions and there is no defensible way to pick
one.

Every result is verified against the operation that produced it: `adGroupCriterionID` rejects a
resource name whose customer id is not this client's, and a returned criterion that does not
match the one addressed is reported UNCONFIRMED rather than as success. A short or malformed
mutate response is UNCONFIRMED for the same reason — the mutations may have been applied, and
this path changes spend, so the caller is told to verify in Google Ads rather than to assume
nothing happened.

**Every criterion's TYPE is resolved before the mutate, and only a POSITIVE KEYWORD is
actionable** (`resolveKeywordCriteria`, LFXV2-2641). Matching the ad group is not sufficient: an
ad group also holds the userList criteria GA-4 creates, and NEGATIVE keywords share the
`adGroupCriteria` resource family and the same `adGroupId~criterionId` handle. Acting on either
through this endpoint would PAUSE or REMOVE an EXCLUSION, which WIDENS delivery and spend —
the opposite of the endpoint's guarantee, and irreversible for `REMOVE`. The type is established
with the same mechanism the READ path uses rather than a second one: a `keyword_view` query,
Google's type-scoped resource, which is why neither path needs a criterion-TYPE predicate —
the view holds keywords and nothing else. `ad_group_criterion.negative` is selected on top of it
because `keyword_view` carries both POLARITIES, and type alone does not separate them.
**Both paths select it.** The read originally did not, and returned exclusions as ordinary rows
— see the polarity note under the keyword reads above. **An UNRESOLVABLE id FAILS CLOSED; an OMITTED `negative` field does not** —
these are different facts and conflating them broke the happy path. `negative` is a proto bool,
so protobuf JSON omits it whenever it is false: the omission IS the positive answer, and every
ordinary positive keyword arrives that way. Absence already means "false" in this wire format, so
reading it as "unknown polarity" gives absence a second meaning it cannot carry and refuses every
keyword the read path just handed the caller — a re-read produces the identical omission and
cannot repair it. Only an explicit `negative: true` is refused as an exclusion. An id the view
returns NO ROW for is still refused, which is the guard's whole purpose (a userList criterion
returns no row): refusing costs a caller one re-read, while admitting one risks an irreversible
removal of an exclusion. The
sentinel is `ErrKeywordCriterionNotPositiveKeyword`, which the dispatcher folds onto
`domain.ErrKeywordActionInvalid` — a permanent 400, since no retry turns an audience criterion
into a keyword.

**The type-resolution query carries the READ path's status ALLOW-LIST (`ENABLED`, `PAUSED`),
and it is load-bearing on a mutating path in a way it is not on a read.** `keyword_view` DOES
return `REMOVED` criteria, and Google rejects a pause or removal of an already-removed
criterion as permanently unmutable. Without the predicate such a row resolved as an ordinary
positive keyword, the mutate was sent, and that PERMANENT rejection came back through the
transport-error path as a RETRYABLE 503 — a stale handle advertised as "try again", which no
number of retries can repair. Excluded by the allow-list the row is simply absent, so the
fail-closed `!ok` arm answers `ErrKeywordCriterionNotPositiveKeyword` (a 400) before anything
is mutated. Enumerating the live states rather than excluding `REMOVED` also default-denies
`UNSPECIFIED`, `UNKNOWN` and the `""` an omitted proto field decodes to — the same reasoning
`GetKeywordPerformance` documents. Asserting only that a query lacks `!= 'REMOVED'` does not
pin this: a predicate-free query satisfies that assertion, which is the trap the settings
readback's REMOVED test fell into.

**Both ids are PARSED as positive `int64`s, not merely length-capped**
(`ValidateKeywordActions` calls `canonicalCampaignID`). Digits-only bounds the character
class; it does not bound the VALUE, and neither does a digit count. `math.MaxInt64` has
NINETEEN digits, so a twenty-digit id is digits-only, injection-safe, within a
twenty-digit cap, and still incapable of naming a criterion that exists — and so are
`"9999999999999999999"` (nineteen digits, above `math.MaxInt64`), `"0"`, and the
leading-zero spelling `"0305729261"`. A cap of 20 was the first attempt here and it
admitted every one of those. Left unrefused they were interpolated into the
type-resolution request, and Google's PERMANENT rejection came back through the read arm
that classifies as a RETRYABLE 503 — a handle no number of retries can make valid,
advertised as "try again". `canonicalCampaignID` is REUSED rather than reimplemented, so
the two surfaces cannot drift, and its round-trip through `ParseInt`/`FormatInt` collapses
every spelling to one — which is also what makes the `adGroupID+"~"+criterionID` keys
compare criteria rather than text. `maxKeywordIDLen` is now 19 and exists only to keep the
design's `MaxLength` honest so Goa refuses the clearly-impossible ids before a handler
runs; the design was corrected from 20 in the same change. Two general rules meet here:
**a non-HTTP validation backstop must mirror every design constraint, not only the
character class** — whatever it lets through becomes an upstream error this code then has
to classify, and a permanent upstream fault reached by a request we should have refused
reads as transient — and **a proxy for a constraint is not the constraint: if the real
rule is "names a positive int64", check that, because a digit count only approximates it
and the gap is where the invalid ids live.**

**Ambiguous outcomes are marked STRUCTURALLY, not just in prose, and so is the ABSENCE of
ambiguity.** `unconfirmedKeywordError` implements `Unconfirmed() bool`, the behavioural
interface `IsOutcomeUnconfirmed` and the service's `classifyKeywordActionError` match with
`errors.As`. Its counterpart `notAttemptedError` implements `NotAttempted() bool` and is checked
FIRST, because ambiguity is otherwise INFERRED from an error's shape: `createOutcomeAmbiguous`
reads any `transportError`, 5xx or exhausted 429 as "the mutation may have committed", which is
the right default for a MUTATE and the wrong answer for the pre-mutation `resolveKeywordCriteria`
read, which fails with those same shapes before `adGroupCriteria:mutate` is ever built. Marking
the read arm keeps a failed type-resolution reported as a definite, safely retryable failure
instead of sending the caller to verify a batch that was never sent. It changes only the
CONFIRMED/UNCONFIRMED axis, not retryability — a failed read is still a 503. This matters because the two 2xx
arms (malformed/short response, mismatched resource name) carry NO underlying error for
`createOutcomeAmbiguous` to classify — labelling them "UNCONFIRMED" in the message alone left
them falling through to the DEFINITE-failure 503 ("could not be applied"), telling a caller to
retry a batch Google may already have run. The dispatcher preserves the marker rather than
returning the client error raw, mirroring the status-toggle path.
