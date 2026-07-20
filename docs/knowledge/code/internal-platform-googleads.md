---
type: "Go Package"
title: "internal/platform/googleads"
description: "Google Ads API REST client: OAuth2 refresh-token auth, request layer, GAQL search (GA-1), and PAUSED campaign creation (GA-2)."
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

## Campaign creation (GA-2)

`CreateCampaign` (in `campaign.go`) creates a PAUSED search campaign as two
sequential `:mutate` calls: `campaignBudgets:mutate` (a non-shared `STANDARD`
budget, `amountMicros` = budget × 1,000,000) then `campaigns:mutate` (status
`PAUSED`, `advertisingChannelType` `SEARCH`, referencing the budget's
`resourceName`, with a dependency-free `manualCpc {}` bidding strategy — a broker
can't assume conversion tracking, which `maximizeConversions` requires). The
campaign create also sets `containsEuPoliticalAdvertising:
DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` — v23 REQUIRES this on every create
(omitting it fails with `FieldError.REQUIRED`, and since 2026-04-01 an account with
any undeclared campaign has ALL mutate calls rejected). Both resource ids are
surfaced (`campaignBudgetId` + `campaignId`) via `firstResourceName`, which decodes
`results[0].resourceName` and returns both the resource name and its trailing-id
segment. It errors when the body is malformed, carries no result/resourceName, OR
the resourceName is present but MALFORMED (e.g. `customers/1/campaigns/` or
`noslash`) such that no id can be extracted — accepting a present-but-malformed
name would let creation continue with an empty, unreconcilable id (or report
success with a blank id), so it is treated as UNCONFIRMED like the no-resourceName
case. Between the two calls, if the
caller's context is already done, the campaign `:mutate` is skipped and the created
budget is returned as a reconcilable partial rather than fired on a dead context.

Input is validated up front, before any paid `:mutate` call: the budget must be
finite (NaN/Inf rejected — NaN passes every ordered comparison, so it would
otherwise slip through and create a $0 budget) and must round to a positive
`amountMicros` (a sub-micro value like 0.0000001 is > 0 but converts to 0 micros);
and BOTH Project AND EventName must be non-empty (independently — mirrors the
meta/twitter/reddit clients). Project is the canonical attribution key the data
pipeline parses out of the campaign name, so a campaign with only one segment is
mis-attributed, not just "less descriptive". Caller-supplied name segments are
sanitized (`sanitizeNamePart`) to strip the `|` delimiter before composing, so a
raw `|` in Project/EventName/NameSuffix can't inject extra pipe-fields and break
the name-based attribution/reconciliation that splits on `|`.

The composed name is length-validated against **per-entity** limits, not a single
shared limit: Google Ads v23 permits `CampaignBudget.name` up to 255 chars but
`Campaign.name` only 128. Collapsing them (validating the campaign name against
255) would let a 129–255-char campaign name pass preflight and then be rejected by
the paid `campaigns:mutate` call AFTER the budget was already created — an
avoidable orphan. Both names are validated before either `:mutate`.

Because `:mutate` has NO idempotency key, a blind retry double-creates. So every
create outcome is classified: an ambiguous failure (a mutating 3xx/5xx `apiError`,
a mutating **429** — `doRequest` deliberately does NOT retry a non-idempotent 429
because the throttled request may already have committed — a `transportError`, or a
2xx with no `resourceName`) is reported UNCONFIRMED (verify before retrying) with a
partial result carrying whatever exists so an orphan is reconcilable. A definite
4xx means only THAT `:mutate` was rejected — for a campaign-create 4xx the budget
from the first mutate still exists, so the partial carries `campaignBudgetId` for
reconcile/cleanup (it is NOT "nothing was created" overall).
`createOutcomeAmbiguous` (5xx/429/transport always; 3xx only on a mutating method),
`isDuplicateBudgetNameErr` (a 4xx `CampaignBudgetError.DUPLICATE_NAME`), and
`isDuplicateCampaignNameErr` (a 4xx `CampaignError.DUPLICATE_CAMPAIGN_NAME` — a
DIFFERENT code from the budget's) drive this: a retry with a stable `NameSuffix`
that collides on the family-appropriate duplicate code is surfaced as
already-exists (reconcile by name). Error codes are parsed from
`error.details[GoogleAdsFailure].errors[].errorCode` (a single-key category→enum
object) from the FULL error body in `doRequest` and retained on
`apiError.ErrorCodes` — NOT re-parsed from the truncated `apiError.Body` (a real
Google error JSON exceeds `maxErrorBodyChars`, so parsing the truncated snapshot
would silently drop the codes). Codes are length/count bounded; the raw body is
never surfaced by `apiError.Error()`. Composed names are deterministic (`LFX |
<kind> | Project | Event | NameSuffix`) so a retry with the same `NameSuffix`
collides on the duplicate code rather than double-creating. NOTE: a non-shared
budget's name syncs to the campaign name once attached; Google does NOT document
that the original budget name is then freed for reuse, so for at-most-once retries
callers should pass a stable-per-logical-campaign `NameSuffix` (which makes the
retry collide on `DUPLICATE_NAME`) rather than relying on name reuse.

## Scope

GA-1 is the scaffold (auth + request layer + GAQL search); GA-2 is campaign
creation (`:mutate`). Metrics/keywords/audience reads, keyword actions, and the
orchestrator dispatcher follow in GA-3..GA-5.
