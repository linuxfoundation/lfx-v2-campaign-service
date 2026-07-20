---
type: "Go Package"
title: "internal/platform/hubspot"
description: "HubSpot API client (email-channel scaffold): static private-app bearer auth, request layer with 429 retry, typed body-free errors."
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
drives HubSpot's email surface: marketing-email clone/patch/content-set, CRM
contact-list CRUD, and event-definition lookups. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database. In production the bearer token comes from a
decrypted stored `hubspot_connections` connection (`private_app_token`).

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
  renders only method/path/status — the response body is retained for internal
  classification but NEVER surfaced (a HubSpot error envelope can quote request
  material). A round-trip failure after the request was plausibly sent, or a 2xx
  whose body can't be read, is a `transportError` (AMBIGUOUS); its `Error()` peels
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

## Scope

This is the SCAFFOLD (auth + request layer). The HubSpot operations —
marketing-email search/get/clone/patch/set-content/set-send-list (LFXV2-2779) and
CRM-lists + event-defs search/get/create/update-filter-branch (LFXV2-2780) — build
on `doRequest`, each mutating op classifying its outcome per the ambiguity contract.
