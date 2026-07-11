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
objective->parameter mapping (awareness/traffic/engagement/leads/conversions;
`leads` runs a website-leads campaign — OUTCOME_LEADS optimizing for LINK_CLICKS
to the registration URL — rather than an on-Facebook instant-form flow),
placement/promoted-object building, and UTM URL construction that preserves any
URL fragment.

Inputs are validated up front, before any mutating call: geo targets are checked
against ISO 3166-1 alpha-2 and comprehensively-sanctioned countries are
excluded; budgets are bounded (rejecting sub-cent-rounds-to-zero and
overflow-scale values, noting the amount is interpreted in the ad account's
currency); dates are parsed strictly (impossible calendar dates rejected) and a
past start date is refused, with a same-day ad-set `start_time` nudged to
now+buffer. `doRequest` retries HTTP 429 and Graph rate-limit envelope codes
(4/17/32/341/613) with bounded backoff, draining the body before close, and a
truncated response body is surfaced rather than reported as a false success.

See [internal/platform/meta](../../../internal/platform/meta).
