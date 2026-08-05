---
type: "Go Package"
title: "internal/platform/linkedin"
description: "LinkedIn Marketing API client: OAuth2 dark-post campaigns (Campaign Group -> Campaign -> Dark Post -> Creative) with targeting and up-front validation."
resource: "internal/platform/linkedin"
tags:
  - platform-client
  - linkedin
  - linkedin-ads
  - oauth2
  - go-package
  - metrics
timestamp: "2026-08-05T00:00:00Z"
---

# internal/platform/linkedin

Package linkedin provides a Go client for the LinkedIn Marketing API, ported
from the upstream TypeScript `linkedin-ads.service.ts` client. Credentials and
the full runtime config are injected via `NewClient`; the client never reads the
process environment or files.

Authentication is a Bearer access token; every request also sends the
`LinkedIn-Version` and `X-RestLi-Protocol-Version` headers required by the
Marketing API. `CreateCampaign` builds the full sponsored-content hierarchy in
one call — Campaign Group (ACTIVE) -> Campaign (PAUSED) -> Dark Post
(`feedDistribution: NONE`) -> Creative — with targeting assembled from the
runtime config's profile (skills/groups/job-functions) and resolved geo URNs.
Cross-tenant org/account pairing fails closed.

A deliberate divergence from the TS source is that geo resolution is a pure,
cache-only function (no network fallback). Beyond that, the Go port validates
strictly and fails BEFORE any permanent resource is created: budget minimums and
sub-cent/NaN/Inf; registration URL (absolute, http/https, real host); schedule
(malformed/past/reversed); event name and project (trimmed, length-bounded);
targeting facet URNs (numeric ids in the correct namespace); ad-account and org
ids (numeric); geo URNs; and the aliased `cloud-native` profile must exist for
`custom`. Find-or-create uses cursor pagination and propagates transient search errors
(rather than treating them as "not found") to reduce duplicates, but it is
best-effort and NOT atomic across calls: `CreateCampaign` re-POSTs every dark
post and creative on a repeat call, so this package does not itself guarantee
cross-call idempotency. Single-flight IS provided caller-side by the orchestrator's
per-(brief, platform) claim, but that claim is held until explicitly released and is
NOT reclaimed on a timer: a crashed holder strands it until a human acts, which
`StuckDispatchClaims` surfaces (see internal-dispatch.md). Provider-level idempotency
KEYS remain unimplemented (LFXV2-2665). A 429 (idempotent methods only) is retried
with bounded backoff.

## Campaign status toggle

`UpdateCampaignAndCreativesStatus(ctx, campaignID, status)` pauses/resumes a campaign AND
cascades to its creatives, because CreateCampaign leaves creatives DRAFT (and the campaign
PAUSED) — activating only the campaign would not serve (a DRAFT creative never serves; a
creative's EFFECTIVE status is gated by its campaign). Ordering is STATUS-DEPENDENT so a
partial cascade never leaves paid delivery running: on ACTIVATE the creatives are lifted FIRST
(still gated by the paused campaign, so they can't serve yet) and the campaign is flipped
ACTIVE LAST — a creative failure then leaves nothing serving; on PAUSE the campaign gate is
flipped FIRST (delivery stops immediately) and the creatives paused after. Each PARTIAL_UPDATE
is `POST /adAccounts/{acct}/{adCampaigns|creatives}/{id}` (header `X-Restli-Method:
PARTIAL_UPDATE`, body `{"patch":{"$set":{"status"|"intendedStatus": …}}}`). Creatives are
DISCOVERED via the creatives FINDER (`GET /adAccounts/{acct}/creatives?q=criteria&campaigns=List(urn:li:sponsoredCampaign:{id})`,
`X-Restli-Method: FINDER` — LinkedIn persists only a creative count, not ids). Each finder id
(URN or bare-numeric) is normalized to its trailing numeric id and the URN is REBUILT from it,
so a malformed value (a suffix with `?`/`/`/percent-encoding/traversal) is rejected rather than
altering the request path; the URN key is then percent-encoded (`urn%3Ali%3A…`). A stuck/looping
cursor, the page cap, or an element with no usable id FAILS discovery rather than truncating.
On a PAUSE a definite 400 on an in-review creative is tolerated (LinkedIn forbids pausing an
in-review creative; the campaign gate already stopped it). Any other failure after a first
successful mutation is a `partialCascadeError` (Unconfirmed). The narrower `UpdateCampaignStatus`
(campaign only) is retained as the building block. The account id is resolved+validated from
the runtime config (same as create); ids must be numeric. `IsOutcomeUnconfirmed(err)` exposes
the shared ambiguity classifier (and honors the `Unconfirmed()` behavioral interface) so a
caller can tell a maybe-applied outcome (including a partial cascade) from a definite rejection.
`doRequest` gained an optional per-call headers map to carry the `X-Restli-Method` header.

## Metrics read

`GetCampaignMetrics(ctx, accountID, campaignID, window)` implements `service.MetricsReader`
(the type-asserted, optional capability the orchestrator's live-read metrics endpoint
discovers per dispatcher — see `internal/service/orchestrator.go`). `campaignID` is the BARE
NUMERIC id persisted by `campaignFromLinkedIn` (`trailingID` of the creation response's
campaign URN, not a URN) — this method builds the `urn:li:sponsoredCampaign:{id}` and
`urn:li:sponsoredAccount:{acctID}` URNs the Ad Analytics finder itself requires.

The Ad Analytics finder (`GET /adAnalytics`) uses Rest.li 2.0 array/nested-object query
literals — `dateRange=(start:(day:D,month:M,year:Y),end:(...))`, `campaigns=List(urn:...)`,
`accounts=List(urn:...)` — that are not expressible through `url.Values.Encode()` (which would
percent-encode the structural parentheses/colons LinkedIn requires literally), so
`makeAdAnalyticsRequest` builds the raw query string directly and bypasses `doRequest`'s
flat `map[string]string` param model entirely, going through a dedicated
`doAdAnalyticsAttempt` GET path instead. It reuses the client's existing 429 retry policy
(`parseRetryAfter`/`retryBaseDelay`/`maxRetryWait`/`sleepCtx`), the same as `doRequest`'s
idempotent-method retry rule. **UNVERIFIED ASSUMPTION**: the finder name (`q=analytics`),
`pivot=CAMPAIGN`, and `timeGranularity=ALL` are LinkedIn's documented Ad Analytics contract,
flagged in code but not yet verified against a live Marketing API account (mirrors the same
kind of disclosed assumption in `internal/platform/googleads/metrics.go`).

The response's `elements` field is decoded via a `*[]AdAnalyticsElement` pointer so a
missing/null field (malformed response — empty body, `{}`, `null`) is distinguishable from an
explicit `{"elements": []}` (genuine zero activity in the window): a nil pointer is a decode
error, never silently reported as zero metrics. Spend (`costInUsd`, decimal USD) is rounded —
not truncated — into micros (`int64(math.Round(spend * 1_000_000))`).

`dateRangeForWindow` anchors both `this_month` and `last_month` off the first-of-month date
rather than `AddDate(0, -1, 0)` on today's day-of-month, since `time.AddDate` silently
normalizes an invalid day (e.g. subtracting a month from the 31st) into the following month —
that would otherwise shift both windows' boundaries on 29th/30th/31st-of-month days.

See [internal/platform/linkedin](../../../internal/platform/linkedin).
