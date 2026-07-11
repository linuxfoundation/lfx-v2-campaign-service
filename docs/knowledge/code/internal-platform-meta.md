---
type: "Go Package"
title: "internal/platform/meta"
description: "Meta (Facebook/Instagram) Ads Graph API client: Campaign -> Ad Set -> Ad creation with objective mapping and geo/budget validation."
resource: "internal/platform/meta"
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
campaign already exists. To stay fail-safe, `leads` runs a WEBSITE-LEADS campaign
— OUTCOME_LEADS optimizing for LINK_CLICKS to the registration (lead-capture)
URL, with no promoted object — a spendable configuration end-to-end. Full
LEAD_GENERATION / instant-form parity with the TS contract is deferred
(LFXV2-2665).

Inputs are validated up front, before any mutating call: geo targets are checked
against ISO 3166-1 alpha-2 and comprehensively-sanctioned countries are
excluded; per-variant copy is rejected up front when it exceeds Meta's limits
(primary text 125, headline 40, description 30 characters, counted by rune) so
over-limit copy fails before any paid campaign/ad-set exists rather than at
non-fatal creative creation; `CampaignInput.Budget` is denominated in the ad
ACCOUNT's own currency — the client does NO foreign-exchange conversion, so the
caller must pass an amount already in that currency — and it is bounded
(rejecting rounds-to-zero and overflow-scale values) then converted to minor
units by multiplying by the account's Meta `currency_offset`
(`AccountConfig.CurrencyOffset`, default 100; set 1 for zero-decimal currencies
like JPY so the amount is not over-sent 100×) rather than a hardcoded ×100;
dates are parsed strictly (impossible calendar dates rejected) and a
past start date is refused, with a same-day ad-set `start_time` nudged to
now+buffer. `doRequest` retries HTTP 429 and Graph rate-limit envelope codes
(4/17/32/341/613) with bounded backoff, draining the body before close, and a
truncated response body is surfaced rather than reported as a false success.

See [internal/platform/meta](../../../internal/platform/meta).
