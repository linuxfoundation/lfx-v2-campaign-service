---
type: "Go Package"
title: "internal/platform/reddit"
description: "Reddit Ads API v3 client: OAuth2 token refresh and Campaign -> Ad Group -> Ad creation."
resource: "internal/platform/reddit"
tags:
  - platform-client
  - reddit
  - reddit-ads
  - oauth2
  - go-package
timestamp: "2026-07-15T00:00:00Z"
---

# internal/platform/reddit

Package reddit provides a Go client for the Reddit Ads API v3, porting the
upstream TypeScript `reddit-ads.service.ts` client. Credentials and account
configuration are injected via `NewClient`; the client never reads the process
environment.

Authentication uses OAuth 2.0 refresh-token exchange with a cached access token
and an expiry buffer (refresh shortly before the stated expiry). The token
endpoint and API base URL are overridable via functional options for `httptest`.

`CreateCampaign` drives the Campaign -> Ad Group -> Promoted Post (Ad) hierarchy,
creating everything PAUSED with a lifetime budget and objective-aware bid params.
It normalizes geo targets once (trimmed, uppercased) so the ad-group label,
targeting, and region derive from a single source of truth, and computes the
start time up front so a same-day (past midnight-UTC) start is nudged to
now+buffer before the campaign POST. Post URLs are validated by parsing the URL
authority (`reddit.com`/`redd.it` and subdomains only) to prevent host spoofing,
and UTM parameters are merged into the URL query while preserving any fragment.

Supplied subreddit names (`r/golang` or `golang`) are sent to the ad-group
`communities` targeting field as NAMES with the `r/` prefix stripped (and
case-insensitive duplicates removed) -- Reddit's Ads API targets communities by
name, not `t5_` ID, and rejects `t5_` values as "invalid communities". This
matches the reference TS implementation, which sends the stripped names
directly. If the ad-group create returns a 400 "invalid communities" the client
retries once WITHOUT communities (keyword/geo-only) and emits a
communities-skipped warning step, so an invalid subreddit never orphans the
PAUSED campaign.

Because create calls are mutating and paid, a FAILED create is classified by
whether the request may have reached Reddit. `isPreSendDialError` reports a Do
error as pre-send (request definitely NOT sent → clean not-created failure) ONLY
for proofs that no bytes left the client: DNS resolution failure,
connection-refused/no-route, and TLS handshake/certificate errors. The TLS
branches are sound ONLY because the built-in `*http.Client` disables redirect
following (`CheckRedirect` returns `http.ErrUseLastResponse`); otherwise a POST
could be received by Reddit and then redirected to a TLS-broken target, whose
cert error would be misread as pre-send and let a retry duplicate a paid
resource. With redirects disabled, a disallowed 3xx surfaces as a non-2xx
`apiError` (< 500), i.e. a clean not-created failure — Reddit never legitimately
3xx-redirects these calls. Everything else is treated as UNCONFIRMED (may have
been applied): a mid-flight/`Do`-time context error (the per-attempt timeout
wraps the whole round trip, so it can fire after the POST reached Reddit), a
read/decode failure on a 2xx body, and any 5xx are wrapped so callers report
"may exist" and require verification before a manual retry.

See [internal/platform/reddit](../../../internal/platform/reddit).
