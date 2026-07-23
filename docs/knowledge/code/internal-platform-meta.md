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

## Campaign status toggle

`UpdateCampaignStatus(ctx, campaignID, status)` pauses/resumes an existing campaign. Meta's
Graph API updates a node by POSTing to the node id with the changed field, so this is
`POST /{campaignID}` with `{"status": "ACTIVE"|"PAUSED"}` (the same status enum the create
path sets). `StatusActive`/`StatusPaused` are the accepted values; `campaignID` is validated
numeric (`numericIDRE`) before interpolation. `IsOutcomeUnconfirmed(err)` exposes the shared
ambiguity classifier so a caller can tell a maybe-applied outcome (transport/5xx/3xx-mutating)
from a definite rejection.

See [internal/platform/meta](../../../internal/platform/meta).
