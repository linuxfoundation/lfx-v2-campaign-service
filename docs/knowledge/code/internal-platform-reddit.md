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
timestamp: "2026-07-13T23:55:00Z"
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

Supplied subreddit names (`r/golang` or `golang`) are resolved to Reddit Ads
subreddit IDs via the Ads API v3 subreddit targeting lookup
(`GET /targeting/subreddits?query=<name>`) BEFORE the campaign POST, because
community targeting matches on subreddit ID, not name. Resolving up front means a
hard lookup failure (bad endpoint/HTTP/transport or a malformed response) aborts
before any paid resource is created -- no orphaned PAUSED campaign -- and the
lookups cannot consume the past-start buffer. Resolution is best-effort per name:
a name that cannot be resolved (a genuine 2xx not-found) is skipped with a
warning step and the rest proceed; a malformed lookup response aborts rather than
silently dropping targeting. If none resolve, the ad group is still created
without communities (with a communities-skipped warning) rather than orphaning
the campaign. The upstream 400 "invalid communities"
retry-without-communities path is preserved as a backstop.

See [internal/platform/reddit](../../../internal/platform/reddit).
