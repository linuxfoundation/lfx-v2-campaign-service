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
for proofs that no bytes left the client: DNS resolution failure, and
connection-refused/no-route/network-unreachable dial failures. NO TLS error is
treated as pre-send (matching the merged Meta client): a TLS error is not a
reliable pre-send proof for an arbitrary caller-supplied transport — a custom
transport can enable renegotiation, and a wrapping/retrying `RoundTripper` can
surface a `*tls.CertificateVerificationError` or `tls.RecordHeaderError` while
reading a response after forwarding the POST — so both flow to the UNCONFIRMED
path. Redirect following is still force-disabled on every client used, including
one supplied via `WithHTTPClient`: `NewClient` builds a fresh `*http.Client`
carrying the caller's reusable exported fields (`Transport`, `Jar`, `Timeout`) with
`CheckRedirect: noFollow`, rather than mutating the caller's client. The rebuild
depends only on `http.Client`'s documented exported fields (layout-independent) and
never mutates the caller's client, so the override is unconditional — which keeps
3xx handling well-defined. Failures that
prove NEITHER pre-send NOR rejection are treated as UNCONFIRMED (may have been
applied): a 3xx on a MUTATING request (it reached a responder and may have
committed before redirecting — a 3xx on a GET is not a create), a
mid-flight/`Do`-time context error (the per-attempt timeout wraps the whole round
trip, so it can fire after the POST reached Reddit), and a read/decode failure on
a 2xx body are wrapped as `transportError`; a 5xx status is returned as an
`apiError` and classified by status. `createOutcomeAmbiguous` treats all of these
as "may exist", so callers require verification before a manual retry. A definite
4xx is NOT UNCONFIRMED — Reddit
received and REJECTED the request, so nothing was created and the caller gets a
clean failure.

## Campaign status toggle

`UpdateCampaignAndChildrenStatus(ctx, campaignID, adGroupID, adID, status)` pauses/resumes a
campaign AND cascades to its child ad group + ad, because the create path PAUSES all three
entities — toggling only the campaign to ACTIVE would leave the ad group/ad PAUSED and the
campaign would not serve. Each entity is set via `PATCH /ad_accounts/{accountID}/{entity}/{id}`
with `{"data":{"configured_status": "ACTIVE"|"PAUSED"}}` — the same envelope + `configured_status`
field the create path sets (`configured_status` is the advertiser-set state, distinct from the
read-only `effective_status`). The cascade is parent-first (campaign → ad group → ad) so an
intermediate failure never leaves a servable child under a paused parent; an empty child id
(a degraded create) is skipped and only the campaign is toggled. `StatusActive`/`StatusPaused`
are the two accepted values. Every id is validated with the letters/digits/underscores guard
(rejecting `/`, `?`, `#`) before interpolation. A PATCH is idempotent, so `request()` may
safely retry it on a 429. (`UpdateCampaignStatus(ctx, campaignID, status)` toggles the campaign
alone and is retained as the per-entity building block / for callers with only a campaign id.)
The child ids are read from the persisted `CampaignResult` (`adGroupId`/`adId`) by the reddit
dispatcher's `ToggleStatus`, which now receives the full persisted `*model.Campaign`.

See [internal/platform/reddit](../../../internal/platform/reddit).
