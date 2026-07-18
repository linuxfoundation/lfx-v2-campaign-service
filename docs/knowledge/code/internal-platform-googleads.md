---
type: "Go Package"
title: "internal/platform/googleads"
description: "Google Ads API REST client (GA-1 scaffold): OAuth2 refresh-token auth, request layer, and GAQL search."
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

## Scope

GA-1 is the scaffold (auth + request layer + GAQL search). Campaign creation
(`:mutate` flows), metrics/keywords/audience reads, keyword actions, and the
orchestrator dispatcher follow in GA-2..GA-5.
