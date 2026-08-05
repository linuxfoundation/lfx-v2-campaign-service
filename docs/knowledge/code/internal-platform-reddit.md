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
  - metrics
timestamp: "2026-08-05T00:00:00Z"
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
read-only `effective_status`). The cascade ordering is STATUS-DEPENDENT so a partial failure
never leaves paid delivery running unattended: on ACTIVATE it lifts the children first (ad, then
ad group) while the campaign gate is still PAUSED and gates them, then flips the CAMPAIGN gate
LAST — a child failure before the gate flip leaves nothing serving; on PAUSE it flips the CAMPAIGN
gate FIRST (delivery stops immediately), then the children. Two edge cases:
ACTIVATING requires the FULL servable tree: it is REFUSED before any PATCH when EITHER the ad
group id OR the ad id is missing (a reddit create can land a campaign + ad group but no ad — the
no-`PostURL` path returns AdCount 0 / empty AdID — yet still persist as "created"; activating it
would leave nothing to serve, so the caller must not persist "active"). The refusal returns
`domain.ErrCampaignNotProvisioned` (→ 409, a client/state error; the platform is never called).
PAUSING with no child id is fine (pausing the parent already halts delivery) and toggles the
campaign alone. On PAUSE (campaign-gate-first), if the campaign PATCH commits but a later child
PATCH fails, the result is a `partialCascadeError` whose `Unconfirmed()` is true (via
`IsOutcomeUnconfirmed`), so the service reports 503-unconfirmed ("verify before retry") rather than
"not modified" — a retry re-runs the idempotent cascade. (On ACTIVATE, children-first, a child
failure occurs BEFORE the campaign gate opens, so nothing is serving and it is a plain error, not
a `partialCascadeError` — `partialCascadeError` is PAUSE-only.) `StatusActive`/`StatusPaused`
are the two accepted values. Every id is validated with the letters/digits/underscores guard
(rejecting `/`, `?`, `#`) before interpolation. A PATCH is idempotent, so `request()` may
safely retry it on a 429. (`UpdateCampaignStatus(ctx, campaignID, status)` toggles the campaign
alone and is retained as the per-entity building block / for callers with only a campaign id.)
The child ids are read from the persisted `CampaignResult` (`adGroupId`/`adId`) by the reddit
dispatcher's `ToggleStatus`, which now receives the full persisted `*model.Campaign`.

## Metrics reads — UNVERIFIED, best-effort contract (LFXV2-2995)

`GetCampaignMetrics(ctx, campaignID, window)` reads impressions, clicks, and spend for a
campaign. **Unlike every other client in this package (and unlike the Meta/LinkedIn/X
metrics clients built against public API docs), Reddit's v3 reporting/metrics endpoint has
NO public documentation at all** — it lives behind Reddit's gated developer portal and a
private Postman collection (postman.com/reddit-ads-api). This was investigated and recorded
as BLOCKED on LFXV2-2995: third-party integrations (Supermetrics, Domo, Unified.to, Bright
Analytics) prove the capability exists but publish neither the request nor response shape.

The implementation is inferred ONLY from this package's own proven, already-working v3
conventions: `POST /ad_accounts/{account_id}/reports` with the same `{"data": {...}}`
envelope every create/toggle call uses, a `campaign_ids`/`breakdowns`/`fields` body, and a
response `{"data": [{"campaign_id", "impressions", "clicks", "spend"}]}` array — `spend`
assumed to be a decimal-currency string (like Meta/LinkedIn's reporting convention) rather
than pre-scaled to micros (like X's `billed_charge_local_micro`). **None of this has been
verified against a live Reddit Ads account or Reddit's real (gated) contract**, and every
field name, the request shape, and the response shape should be treated as a placeholder to
correct once `adsapi-partner-support@reddit.com` or Postman collection access confirms the
real endpoint. `dateRangeForWindow` maps the shared `model.MetricsWindow` literal
(`today`/`last_7_days`/`last_30_days`/`this_month`/`last_month`) to a `YYYY-MM-DD` start/end
pair, handling the last-month-at-month-end boundary the same way the LinkedIn client's
`dateRangeForWindow` does. `ErrInvalidCampaignID`/`ErrUnsupportedWindow` are typed sentinels
(`errors.Is`-discriminable), matching the LinkedIn/X metrics clients' convention. An explicit
empty `data` array is real "no activity", not an error — but a missing/malformed `data`
field is NOT: `json.Unmarshal` on the resulting nil/empty bytes fails decode, and that
surfaces as the same transport/decode error used for any other malformed metrics response,
not as zero-activity. CTR is clicks/impressions, 0 when impressions is 0.

The `model.MetricsWindow`/`model.CampaignMetrics` types and the `service.MetricsReader`
interface this depends on are NOT yet on `main` (GA-5, PR #70, is still an open, unmerged
epic-stacked PR) — this branch was cut from `main` directly and adds its own copy of that
scaffold, mirroring the same pattern already used on the Meta/LinkedIn/X metrics branches. No
live `Orchestrator.ReadCampaignMetrics` type-assertion caller exists on this branch yet; that
wiring lands when the metrics-parity branches are reconciled.

See [internal/platform/reddit](../../../internal/platform/reddit).
