---
type: "Go Package"
title: "internal/platform/reddit"
description: "Reddit Ads API v3 client: OAuth2 token refresh, Campaign -> Ad Group -> Ad creation, campaign metrics reads built to Reddit's public OpenAPI spec (gated pending a live-account run)."
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
directly.

TWO targeting dimensions can be rejected wholesale, and both are handled by one
retry. Communities are rejected when a subreddit name does not resolve;
`interests` are rejected because Reddit wants opaque ids (`technology_v3`) while
the brief generator produces human labels ("Machine Learning"), so in practice
every AI-generated brief sends values Reddit refuses. Either rejection returns a
400 that would otherwise ORPHAN the PAUSED campaign created moments earlier.

`baseTargeting` deliberately carries NEITHER dimension and is the retry payload;
`optionalTargeting` adds both for the first attempt. A 400 naming either one
therefore drops BOTH on a single retry rather than chaining two — retrying per
dimension sends a second doomed request, and every extra ad-group POST is another
chance to create one nothing points at. The Steps trail distinguishes what Reddit
REJECTED from what was proactively dropped alongside it, because an operator
re-adding targeting by hand needs to know which is which, and the two are re-added
in different places in Reddit Ads Manager.

This makes the failure SURVIVABLE, not the targeting correct: a campaign whose
interests and communities were dropped runs on keywords and geo alone. Resolving
labels to Reddit's ids is tracked separately (LFXV2-3261).

A conversion pixel is required by Reddit on EVERY campaign create, not only for
the `conversions` objective its documentation describes. It is read from the
CONNECTION (`conversion_pixel_id`, migration 000025) rather than per campaign,
because it identifies the advertiser: one pixel per ad account, the same for every
campaign created through it. A per-campaign value still overrides when present. A
create with none configured is refused BEFORE any upstream call, with a message
naming the connection — an operator reading it is looking at the campaign they
were editing, which is not where the fix lives.

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

## Metrics reads — contract from Reddit's public OpenAPI spec (LFXV2-3282)

`GetCampaignMetrics(ctx, campaignID, window)` reads impressions, clicks, and spend for a
campaign. **It is unreachable in a default deployment**: the dispatcher above it
(`RedditDispatcher.ReadMetrics`) is gated on `REDDIT_METRICS_ENABLED == "true"` and otherwise
returns `domain.ErrMetricsUnsupported`.

The request and response shapes come from Reddit's **official public OpenAPI document**
(`https://ads-api.reddit.com/api/v3/openapi.json`, linked as "Download Specs" from
`https://ads-api.reddit.com/docs/v3/`), operation `POST /ad_accounts/{ad_account_id}/reports`,
schemas `Report` and `ReportMetric`. Reddit's own introduction states the Ads API "is open to
all developers and does not require allowlisting or approval from Reddit to access".

**This supersedes the LFXV2-2995 BLOCKED finding**, which recorded that Reddit published no
public documentation for v3 reporting and that the shape here was a guess inferred from this
client's own conventions. That finding is no longer accurate. Reading the spec falsified four
of the five things previously guessed:

| Element | Previous guess | Spec |
| --- | --- | --- |
| Path + method | `POST /ad_accounts/{id}/reports` | correct — the one guess that held |
| Campaign scoping | `campaign_ids` array | `filter` string DSL, `campaign:id==<id>` |
| Field names | lowercase `impressions`, `clicks`, `spend` | UPPERCASE enums `IMPRESSIONS`, `CLICKS`, `SPEND`, `CAMPAIGN_ID` |
| Response `data` | bare array of rows | object with a `metrics` array (plus `pagination`) |
| `spend` | decimal-currency string, scaled by 1e6 | `int64` already in **microcurrency** |
| `starts_at`/`ends_at` | bare `YYYY-MM-DD` | `YYYY-MM-DDTHH:00:00Z`, hourly granularity only |

The request schema sets `additionalProperties: false`, so the previous `campaign_ids` key
would have been rejected outright. `spend` being microcurrency (not a decimal string) is the
correction with the largest blast radius: the old code multiplied by 1e6, which against the
real contract would have reported every spend figure one million times too large had it
decoded at all. Note "microcurrency" means the **ad account's own billing currency**, which
this client does not read — `CostMicros` is therefore micros of an unspecified currency, as it
is for X, and must not be summed across platforms.

`CAMPAIGN_ID` is requested as a **field**, not merely a breakdown, so every row carries the id
the provenance check verifies against. `breakdowns` is omitted so Reddit aggregates over the
whole window; the accumulation loop still sums correctly if that is wrong. `time_zone_id` is
omitted because the spec documents it as defaulting to UTC, which is the zone
`dateRangeForWindow` renders in.

**What remains unverified**: no request has been made against a live Reddit ad account (this
repository holds no Reddit credentials), which is why the gate stays on. A schema cannot
express whether a campaign with no activity yields an empty `metrics` array or a row of
explicit zeros, nor whether `ends_at` is inclusive of its final hour. Both readings are handled
without a wrong answer, and both are recorded at their site rather than assumed away.

### Refusing rather than reporting a plausible number

Every metric field is decoded as a **pointer**, and a missing or null field is an error. The
spec types `impressions`, `clicks`, and `spend` as `["integer","null"]`, so Reddit may send an
explicit null; a value field would decode that to `0`, which is indistinguishable from a
campaign that genuinely served nothing. This is stricter than the googleads client, which
treats an empty metric as a real zero — that is correct there because Google Ads is
*documented* to omit zero-valued metrics, and Reddit's spec documents no such behaviour.

The refusal names which requested fields were absent, so a wrong assumption is diagnosable in
one read rather than by debugging a decoder. No upstream value, id, or account id is echoed
into an error; errors report the bare `reports` path because the account id is interpolated
into the real one.

Response handling distinguishes:
- An **explicit empty `metrics` array** is real "no activity" (zero metrics), not an error.
- A **null or missing `data`**, a `data` that is not an object, or a **null/missing `metrics`
  array** are all decode errors. Consumers must not assume missing data means zero activity.

Rows are validated before accumulation: the row's `campaign_id` must match the requested
campaign (the `filter` is not trusted to have scoped the report), counters must be
non-negative, and a row reporting clicks with zero impressions is rejected as impossible —
the same guard the LinkedIn client applies. Checked additions stop a running total from
wrapping past `MaxInt64`. CTR is derived from the **totals**, not read from the row's own
`ctr` field nor averaged per row.

`dateRangeForWindow` maps the shared `model.MetricsWindow` literal
(`today`/`last_7_days`/`last_30_days`/`this_month`/`last_month`) to the required hourly
timestamp pair, anchored to the UTC calendar date and handling the last-month-at-month-end
boundary from a first-of-month anchor (never `AddDate(0,-1,0)`, which normalizes an invalid
day into the following month). The end bound is the `23:00` hour of its day, since midnight
would ask for a range stopping as the final day begins. `ValidateMetricsWindow` is
package-level and clock-free so a caller can reject a window without credentials, and it is
checked before the account is resolved so a permanent 400 is not masked as a retryable
connection failure. `ErrInvalidCampaignID`/`ErrUnsupportedWindow` are typed sentinels
(`errors.Is`-discriminable).

The campaign-id charset guard now protects two interpolation sites, not one: the URL path and
the `filter` DSL, where a comma would split one filter term into two and silently widen the
report's scope.

The `model.MetricsWindow`/`model.CampaignMetrics` types and the `service.MetricsReader`
interface this depends on come from the platform-agnostic metrics foundation
(LFXV2-2997, merged to `main`), not from a per-branch copy.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` reddit adapter (see [internal/dispatch](internal-dispatch.md))
interprets OAuth2 (clientId/secret/refreshToken) credentials; AccountID comes from the
connection.

It implements `StatusToggler`: `resolveRedditClient` (shared with `Dispatch`, so a create
and a toggle accept exactly the same connections) builds the client, then
`client.UpdateCampaignAndChildrenStatus` PATCHes `configured_status` on the campaign AND its
child ad group + ad (read from the persisted `CampaignResult`) — because the create path
PAUSES all three, so toggling only the campaign would not serve.

See [internal/platform/reddit](../../../internal/platform/reddit).
