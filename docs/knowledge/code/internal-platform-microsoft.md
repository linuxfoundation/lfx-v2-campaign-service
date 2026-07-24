---
type: "Go Package"
title: "internal/platform/microsoft"
description: "Microsoft Advertising (Bing Ads) Campaign Management REST v13 client scaffold: OAuth2 refresh-token auth, the request layer with 429 retry, and error classification (MS-1). Campaign creation lands in MS-2."
resource: "internal/platform/microsoft"
tags:
  - platform-client
  - microsoft-ads
  - bing-ads
  - oauth2
  - go-package
timestamp: "2026-07-22T00:00:00Z"
---

# internal/platform/microsoft

Package microsoft provides a Go client for the Microsoft Advertising (Bing Ads)
Campaign Management API, backing the UI's Microsoft/Bing ad channel. It speaks the
**REST** transport directly (v13) rather than the legacy SOAP Campaign Management
service, so the client matches the meta/reddit/twitter/linkedin/googleads structure
and avoids a SOAP dependency. Credentials and account configuration are injected via
`NewClient`; the client never reads the process environment.

Naming note: the platform key surfaced to callers (`CampaignResult.Platform`, every
error prefix) is `microsoft-ads`, even though the live REST host is the legacy
`campaign.api.bingads.microsoft.com` domain — "Bing Ads" and "Microsoft Advertising"
are the same platform.

## Auth

Like Google Ads, Microsoft auth is richer than a single Bearer token. `Credentials`
carries the OAuth2 `client_id`/`client_secret` + `developer_token` + `refresh_token`;
`AccountConfig` carries the `account_id` (digits only, sent as `CustomerAccountId`)
and an optional `customer_id` (digits only, sent as `CustomerId` when set). Every
call sends the bearer access token, the `DeveloperToken` header, `CustomerAccountId`,
and — when set — `CustomerId`. The refresh-token→access-token exchange runs against
the Microsoft identity platform (`login.microsoftonline.com/common/oauth2/v2.0/token`,
scope `https://ads.microsoft.com/msads.manage offline_access`) and is coalesced with a
single-flight leader/follower (the token mutex is never held across the network call,
and the refresh runs on a `WithoutCancel`-detached context so one caller's
cancellation can't tear down a shared refresh). The OAuth response body is never
echoed into errors (it can carry the `client_secret`/`refresh_token` back).

## Request layer

`doRequest` applies the repo's standard discipline: no-follow redirects (on a shallow
copy so a supplied client isn't mutated), bounded response reads (`maxResponseBytes`),
and the pre-send (`isPreSendDialError`) vs ambiguous (`transportError`) vs definite
(`apiError`, status-only `Error()`) classification. `account_id`/`customer_id` are
validated digits-only at the request choke point (a header-injection guard). The
access token is fetched INSIDE the retry loop so a long 429 backoff can't leave a
resumed retry using an expired token, and each attempt is wrapped in a per-call
`context.WithTimeout(msAdsRequestTimeout)` so a custom `WithHTTPClient` with
`Timeout==0` can't hang indefinitely.

A rate-limited (429) **idempotent** call is retried up to `retryMax` times with a
bounded backoff honoring `Retry-After`; retry eligibility is an explicit parameter
(not the HTTP method) so a mutating create (a POST) is NOT retried on 429 — the
throttled request may already have committed (double-create risk). A server-declared
`Retry-After` that exceeds `maxRetryWait` returns the `overCapRetryAfter` sentinel and
ABORTS rather than clamp-and-retry into a guaranteed second 429; the delta-seconds
form is compared in seconds BEFORE converting to a `time.Duration` (so a huge value
can't overflow the multiply and wrap to a short wait), and `parseNonNegativeInt`
rejects overflow before each multiply-add (a bare `n<0` check misses a wrap past zero).

A read failure or an oversized body is classified STATUS-AWARE (`statusAwareReadError`):
a 2xx (the mutation may have committed but the result is unreadable) is an ambiguous
`transportError`, while a known non-2xx keeps its status as an `apiError` so
definite-4xx and 429-retry classification survive; an oversized 429 still follows the
retry/over-cap path. `transportError` wraps its cause UNEXPORTED and renders it via
`safeCause` (a fixed URL-free vocabulary) so a `*url.Error`'s embedded request URL
can't leak into a persisted campaign step.

## Error-code parsing

Microsoft reports errors two ways: a non-2xx `ApiFaultDetail` body (top-level
`Code`/`ErrorCode` + nested `OperationErrors` for request-level faults and
`BatchErrors` for per-list-item faults), and — on a 200 with per-entity failures — a
`PartialErrors` array (see MS-2). `parseErrorCodes` extracts the machine-readable codes
from ALL of these (`Errors`/`OperationErrors`/`BatchErrors`/`PartialErrors`) into
`apiError.ErrorCodes` (from the FULL body before truncation), tolerating Microsoft's
string-or-number `Code` via `codeString`. Visiting `BatchErrors` matters because a
duplicate/field error on one item of a batch mutate lands there, not in
`OperationErrors`. Codes are length/count bounded; the raw body is never surfaced by
`apiError.Error()`.

## Scope

MS-1 is the scaffold (auth + request layer + error classification). MS-2 adds PAUSED
campaign creation (`campaign.go`). The orchestrator dispatcher (register
`microsoft-ads`, use the stored `connection-microsoft-ads` credential) follows in MS-3.
