---
type: "Go Package"
title: "internal/platform/meta"
description: "Meta (Facebook/Instagram) Ads Graph API client: Campaign -> Ad Set -> Ad creation with objective mapping and geo/budget validation."
resource: "internal/platform/meta"
tags:
  - platform-client
  - meta
  - facebook-ads
  - graph-api
  - go-package
timestamp: "2026-07-13T19:22:00Z"
---

# internal/platform/meta

Package meta provides a Go client for the Meta (Facebook/Instagram) Ads Graph
API, ported from the upstream TypeScript `meta-ads.service.ts` client.
Credentials and account configuration are injected via `NewClient`; the client
never reads the process environment and uses only the standard library.

Authentication is a Graph API Bearer access token. `CreateCampaign` drives the
Campaign -> Ad Set -> Ad(s) hierarchy, creating everything PAUSED, with
objective->parameter mapping (awareness/traffic/engagement/leads/conversions),
placement/promoted-object building, and UTM URL construction that preserves any
URL fragment.

The `leads` objective INTENTIONALLY DIVERGES from the `@lfx-one/shared` TS
contract (`campaign.constants.ts` maps leads -> LEAD_GENERATION with a page_id
promoted object). LEAD_GENERATION optimization requires the ad creative to carry
an on-Facebook instant lead form (`lead_gen_form_id`), which this client does not
construct — it only builds a website-click creative pointing at the registration
URL. Adopting LEAD_GENERATION would fail at ad-set/ad creation, after the paid
campaign already exists. To stay fail-safe, `leads` runs an interim WEBSITE-TRAFFIC
campaign — OUTCOME_TRAFFIC optimizing for LINK_CLICKS to the registration
(lead-capture) URL, with no promoted object. OUTCOME_TRAFFIC is used (not
OUTCOME_LEADS) because OUTCOME_LEADS + LINK_CLICKS requires a `pixel_id` +
`custom_event_type` that this interim flow does not supply — that pairing would
create the campaign then fail at the ad set, orphaning it. OUTCOME_TRAFFIC
supports LINK_CLICKS with no pixel requirement, so the flow is spendable
end-to-end. Full LEAD_GENERATION / instant-form (or OUTCOME_LEADS + pixel) parity
with the TS contract is deferred (LFXV2-2665).

Inputs are validated up front, before any mutating call: geo targets are checked
against ISO 3166-1 alpha-2 and comprehensively-sanctioned countries are
excluded; per-variant copy is rejected up front when it exceeds Meta's limits
(primary text 125, headline 40, description 30 characters, counted by rune) so
over-limit copy fails before any paid campaign/ad-set exists rather than at
non-fatal creative creation; `CampaignInput.Budget` is denominated in the ad
ACCOUNT's own currency — the client does NO foreign-exchange conversion, so the
caller must pass an amount already in that currency — and it is bounded
(rejecting rounds-to-zero and overflow-scale values) then converted to minor
units by multiplying by the account's minor-unit offset
(`AccountConfig.CurrencyOffset`) rather than a hardcoded ×100. That offset is
DERIVED from the account's ISO 4217 currency code, not fetched: the Meta AdAccount
node exposes only `currency` (the ISO code) — it does NOT expose a
`currency_offset` field (only the separate Currency node does). CreateCampaign
maps the code through an AUTHORITATIVE supported-currency table
(`currencyMinorUnitOffset`), which is the single source of truth: the zero-decimal
currencies (JPY, KRW, CLP, VND, and the rest of the standard set) map to 1, and the
enumerated two-decimal currencies (USD, EUR, GBP, and the other supported majors)
map to 100. There is NO fall-through default — a code absent from the table
(blank, or a well-formed-but-unknown code such as `ZZZ`) is treated as unsupported.
The offset is never guessed: when `AccountConfig.CurrencyOffset` is unset (zero) — the normal
case for a dispatch built from a persisted connection, which carries only
account/page/app IDs — CreateCampaign fetches the account's `currency` (ISO code)
from the ad-account object during the account preflight, BEFORE any mutating call,
derives the offset from it, and fails closed if the currency is unknown or absent.
Silently defaulting to 100 would encode a zero-decimal-currency
(JPY/KRW/CLP) budget 100× too high, and a warning after resource creation cannot
prevent that budget from being activated. The account currency is
authoritative: a caller MAY set a positive `CurrencyOffset` as a FALLBACK, but if
the preflight returns a recognized currency whose true offset differs, the request
is REJECTED (a stale override would mis-scale the budget). The explicit offset is
only used when the preflight fails or its currency isn't in the supported map. The
preflight GET always runs (it also verifies access). A negative offset is
rejected as malformed. The preflight also reads `account_status`: a
successful GET is not treated as "active" — if the account is in a known-inactive
state (disabled, closed, pending review/settlement, etc.) CreateCampaign fails
BEFORE any mutating call rather than creating a paid campaign Meta would reject
later; an unreported status (0) or any value not known to be bad is allowed
through. `CampaignInput.Project` is
also required (rejected up front if empty/whitespace): the campaign name's
Project segment must be the caller-supplied canonical LFX project slug, so the
client never silently substitutes a placeholder that could mis-attribute a
campaign to the wrong project.
Dates are parsed strictly (impossible calendar dates rejected) and a
past start date is refused, with a same-day ad-set `start_time` nudged to
now+buffer. `doRequest` retries HTTP 429 and Graph rate-limit envelope codes
(4/17/32/341/613/80004) with bounded backoff, draining the body before close, and a
truncated response body is surfaced rather than reported as a false success.
**Creates go through `doCreate`, which suppresses that throttle retry.** The retry
and `createOutcomeAmbiguous` would otherwise hold opposite premises about the same
response: the classifier calls a throttle UNCONFIRMED precisely because Meta may have
committed the node before reporting it, while the retry loop would re-POST the create
on that same signal — producing two campaigns (or ad sets, or ads) with one name
inside a single call, which the start-of-flow name lookup cannot see or reconcile. A
throttled create therefore returns immediately and is classified UNCONFIRMED. What
happens to whatever Meta committed is then an OPERATOR question, not an automatic one:
`findCampaignByName`/`findAdSetByName` is the mechanism that could adopt it, but that
lookup is opt-in and off at every call site today (see "Campaign and ad-set idempotency
by name" below), and nothing re-dispatches a retained partial — so the UNCONFIRMED result
is surfaced for verification in Ads Manager rather than reconciled on a next run. The
suppression is scoped to creates, not to POST as a method — the status-update POSTs
assert a desired state, so repeating them changes nothing and they keep the retry.
Redirect following is force-disabled (a shared `noFollow` `CheckRedirect` policy).
For a `WithHTTPClient`-supplied client, `NewClient` builds a FRESH `*http.Client`
carrying the caller's reusable exported fields (`Transport`, `Jar`, `Timeout`) with
`CheckRedirect: noFollow`, rather than value-copying the caller's client (an
`http.Client` must not be copied after first use — a copy duplicates its internal
mutex while sharing the request-cancellation map). So a 3xx on a mutating POST is
surfaced rather than followed to a different target; `createOutcomeAmbiguous`
classifies a mutating 3xx (gated on the method) and any 5xx/transport failure as
UNCONFIRMED, so a create that may have committed is not blind-retried into a
duplicate. The status is preserved even when the response body is unreadable or
oversized, so an ambiguous outcome is never downgraded to a definite failure.

## Campaign and ad-set idempotency by name

Unlike Google Ads, Microsoft, and LinkedIn — which reject campaign name duplicates
and let a retry discover the existing id via a name query — Meta's Graph API does
NOT enforce campaign-name uniqueness and exposes NO create-idempotency key. A
blind retry would silently create a second paid campaign with the identical name.
To close this window, `CreateCampaign` can run a "poor-man's idempotency"
reconciliation: before POSTing to create a campaign or ad set,
`findCampaignByName` / `findAdSetByName` (using Graph API
`filtering=[{"field":"name","operator":"EQUAL","value":name}]` lookups) check
whether the name already exists. If found, the existing id is reused and the
resource is not re-created.

The lookup is OPT-IN, gated on `CampaignInput.ReconcileByName`, which is false at
every call site today. An unconditional lookup cannot help the case it exists for
and can only harm the cases it does reach. It cannot help because the orchestrator
never re-dispatches a retained partial — it answers "reconciliation required"
(`internal/service/orchestrator.go`) — so no retry arrives here to be reconciled.
It harms because the campaign name is not brief-unique (below), so a different
brief sharing the four name segments would be silently adopted; and because DELETE
frees the (brief, platform) slot LOCALLY without touching the ad platform
(`docs/api-catalog.md`), so the documented delete → re-dispatch flow — the supported
way to correct a campaign created with the wrong budget — would find the old
campaign by name (budget is not a name segment), reuse it and its ad set, and report
success while re-running the old budget. When LFXV2-2665's reconcile path lands it
is the caller, which knows it is resuming a specific dispatch generation, that sets
the flag; the client cannot infer it.

When the lookup does run and fails ambiguously, an UNCONFIRMED partial result is
returned (the resource MAY exist, verify before retrying). Ambiguous is broader than
transport/5xx: a 2xx with no `data` field, a `paging.next` with no cursor, exceeding
the page cap with pagination still pending, a single match whose id is empty or
non-numeric, and context cancellation mid-enumeration all fail closed the same way,
because each leaves a matching campaign possibly present but unexamined. Only a
pre-send failure (dial error) or a definite conflict — a match that is not `PAUSED`,
or whose objective differs from the requested one — is a clean error.

Both names are deterministic, but they are composed differently and carry different
uniqueness guarantees. The CAMPAIGN name is the attribution name (`Events | event |
region | objective | Intent | Social | project | MoFU`) — deterministic for a given
brief, but NOT brief-unique: two briefs sharing event name, region, objective and
project produce the same name. The AD-SET name is only `"<EventName> - <objective
label>"`; it is disambiguated by the campaign it is queried under, not by the name.
Because a name match alone therefore cannot prove a campaign belongs to this brief,
`findCampaignByName` fails CLOSED on anything it cannot pin down: more than one match,
a non-numeric id, a status other than `PAUSED`, or an objective other than the
requested one all abort rather than reuse. Only a single PAUSED campaign with a
matching objective is reused. The ad-set lookup runs only when the campaign was in
fact reused — a campaign created moments ago in the same call cannot already have a
child ad set, so the extra GET would add a failure point with nothing to find.

Both ids the create path produces are gated with `numericIDRE` before they are used
or published. The lookups already gate the ids they return, and every consumer of a
STORED id gates it again, which leaves a FRESHLY CREATED id as the only one that
would otherwise reach `CampaignResult` — and from there durable storage and
`"/{id}/..."` paths on later calls — unvalidated. A 2xx carrying `"123?fields=x"` or
a padded `"123 "` would be persisted now and rejected much later, at a call site with
no idea a campaign was created. Both gates classify the outcome as a malformed
SUCCESS rather than a failure: the resource almost certainly exists (Meta answered
2xx), it simply is not addressable by id, so a name-carrying partial is returned and
the dispatcher keeps the claim instead of freeing it for a duplicating retry.

This builds the client-layer half of LFXV2-2665 for campaign- and ad-set-level
resources. Reaching it on a production retry needs the other half: the orchestrator
re-invoking the dispatcher on a RETAINED claim (today `ClaimCampaignDispatch` hits
`ON CONFLICT DO NOTHING` and the orchestrator returns "reconciliation required"
without calling the dispatcher) AND that caller setting `ReconcileByName`. Until
both land the lookup is exercised only by tests, which is the intended state — the
capability is in place and deliberately unreached rather than on by default.

## Campaign status toggle

`UpdateCampaignAndChildrenStatus(ctx, campaignID, adSetID, status)` pauses/resumes a campaign
AND cascades to its ad set + ads, because Meta's create PAUSES all three — toggling only the
campaign to ACTIVE would leave the ad set/ads PAUSED and the campaign would not serve. Each
entity is updated by POSTing to its node id with `{"status": "ACTIVE"|"PAUSED"}` (Meta's Graph
API updates a node by POSTing to the node id with the changed field; the same status enum the
create path sets). Meta persists the ad set id (in the campaign result) but NOT the individual
ad ids, so the ads are DISCOVERED via `GET /{adSetID}/ads` (paged; an unexpected/looping
cursor, the page cap, or an ad with no usable id fails the discovery rather than silently
truncating). Ordering is STATUS-DEPENDENT (Meta gates a child's serving by its parent's status
— a paused parent is inherited by all children) and DISCOVER-FIRST: the ads are enumerated (a
GET) BEFORE any mutation, then on ACTIVATE the ads are updated, then the ad set, and the campaign
is flipped ACTIVE LAST (every child stays gated by the paused campaign until that flip, so a
mid-cascade failure leaves NOTHING serving); PAUSE flips the campaign gate FIRST then the
children. Ids are validated numeric up front (nothing applied on a bad id); activating with no
ad set id — or with an ad set that has ZERO ads (a degraded broker campaign, since Meta treats
per-variant ad failures as non-fatal at creation) — is refused BEFORE any mutation via
`ErrCampaignNotServable` (the dispatcher maps it to a deterministic 409, so a degraded campaign
converges to a "reprovision" error instead of looping on a transient 503 that re-POSTs the ad
set each retry). A failure once an upstream change may have landed — the pause path, an ambiguous
5xx/transport outcome, OR any activate-path failure AFTER the first ad (or the ad set) was
updated — is a `partialCascadeError` (Unconfirmed). A DEFINITE (4xx) failure BEFORE any child has
been mutated (including a discovery failure, which now precedes all mutation) is a clean failure. `StatusActive`/`StatusPaused` are the
accepted values; ids are validated numeric (`numericIDRE`) before interpolation. The narrower
`UpdateCampaignStatus(ctx, campaignID, status)` (campaign node only) is retained as the
building block. `IsOutcomeUnconfirmed(err)` exposes the shared ambiguity classifier (and honors
the `Unconfirmed()` behavioral interface) so a caller can tell a maybe-applied outcome
(transport/5xx/3xx-mutating, or a partial cascade) from a definite rejection.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` meta adapter (see [internal/dispatch](internal-dispatch.md))
interprets a single OAuth2 accessToken; AccountConfig comes from AccountID (`act_...`) +
`page_id` (REQUIRED by the connection design — the dispatcher needs it to attach the
promoted-object page, so requiring it at connection time surfaces a 4xx instead of a
silent dispatch failure). Budget is in the ACCOUNT's currency (no FX), with an optional
CurrencyOffset.

It implements `StatusToggler` and CASCADES: its create PAUSES the campaign, ad set, and
ads, so `UpdateCampaignAndChildrenStatus` POSTs the status to the campaign, the persisted
ad set id, and each ad DISCOVERED via `GET /{adSetID}/ads` (Meta persists the ad set id
but not the individual ad ids). It needs only the access token, not the page id.

See [internal/platform/meta](../../../internal/platform/meta).
