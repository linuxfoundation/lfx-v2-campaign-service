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
(`AccountConfig.CurrencyOffset`) rather than a hardcoded ×100. When unset (zero)
the offset DEFAULTS to `100` — correct for USD/EUR/GBP and most currencies — and
the assumption is surfaced as a result step; it is not hard-required because the
persisted Meta connection carries only account/page/app IDs (no `currency_offset`),
so requiring it would break every stored-connection dispatch. A zero-decimal
currency account (JPY/KRW/CLP) MUST set it to `1` or its budget is over-sent 100×;
a negative offset is rejected as malformed. `CampaignInput.Project` is
also required (rejected up front if empty/whitespace): the campaign name's
Project segment must be the caller-supplied canonical LFX project slug, so the
client never silently substitutes a placeholder that could mis-attribute a
campaign to the wrong project.
Dates are parsed strictly (impossible calendar dates rejected) and a
past start date is refused, with a same-day ad-set `start_time` nudged to
now+buffer. `doRequest` retries HTTP 429 and Graph rate-limit envelope codes
(4/17/32/341/613) with bounded backoff, draining the body before close, and a
truncated response body is surfaced rather than reported as a false success.

See [internal/platform/meta](../../../internal/platform/meta).
