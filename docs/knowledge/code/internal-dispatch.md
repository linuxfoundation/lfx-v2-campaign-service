---
type: "Go Package"
title: "internal/dispatch"
description: "Per-platform PlatformDispatcher adapters bridging the orchestrator to the channel API clients (six paid ad platforms plus the hubspot email channel), plus the HubSpot audience builder."
resource: "internal/dispatch"
---

# internal/dispatch

Package dispatch holds the per-platform `PlatformDispatcher` adapters that connect the
orchestrator (`internal/service`) to the ad-platform API clients
(`internal/platform/*`). The orchestrator is agnostic to the platforms — it calls
`Dispatch(ctx, brief, provider, config)` on a registered adapter and expects back a
`*model.Campaign` with `PlatformCampaignID`/`Status`/`Result` populated. This package
is the only place that knows both the orchestrator's contract and the concrete
clients, which is why it lives outside `service` (keeping `service` free of platform
imports) and outside each `platform/*` package (avoiding an import cycle).

## Also here: the audience builder

`AudienceBuilder` (`audience_builder.go`) is NOT a `PlatformDispatcher` — it implements
`service.AudienceBuilder` for the audience build (LFXV2-2774). It lives in this package for the
same reason the adapters do: HubSpot credentials are stored per project as encrypted
connections, so it needs the same `credsSource` resolution, and putting it in `service` would
drag platform clients back into the orchestrator's package.

It resolves its HubSpot client once per BUILD, cached on a context scope created by
`BeginBuild` — not on the builder. The distinction matters in both directions: one build creates
several lists which must all land in the same portal (or the master references ids that do not
exist together), but the builder is a container SINGLETON, so caching on it would pin a
credential for the life of the process and keep using a connection that had since been rotated,
revoked or deleted.

## What each adapter does

1. **Resolve credentials** (shared) — `credsSource.resolve(projectID, provider)` does
   the ONE mechanical step common to every platform: `ConnectionReader.Get` then
   `Encryptor.Decrypt`, returning the raw plaintext blob plus the connection's
   non-secret fields (`AccountID`, `ProviderConfig`, `Status`). It does NOT interpret
   the plaintext — credential shapes differ per platform (OAuth2 refresh tokens,
   OAuth1 4-tuples, static bearer tokens), so each adapter unmarshals the blob into
   its own credential struct.
2. **Map inputs** (per-platform) — the adapter reads the brief's event destination
   from its top-level `URL` field (with a nested `registrationUrl` in the opaque JSON
   only as a fallback) and `eventName` from the opaque JSON blobs, plus the
   per-platform config (its OWN nested key — `redditConfig`/`linkedInConfig`/… — out
   of the single `CreateCampaigns` `Input.Config` envelope, via
   `unmarshalPlatformConfig`) onto the client's `CampaignInput`. The **Project** name
   segment is stamped from the authenticated `brief.ProjectID`, NOT from caller JSON
   (it's the data pipeline's attribution join key — see docs/api-catalog.md).
3. **Call the client** and map the result → `model.Campaign` (upstream id, name, the
   provider result blob in `Result`, and a status, since the orchestrator does not set
   a status on success and `UpsertCampaign` writes it verbatim). The success status is
   `created` normally, or **`created_degraded`** when the campaign was created upstream
   but a non-fatal sub-step is incomplete — a promoted-post/ad that failed or is
   unconfirmed, or fewer ads created than requested. The adapter returns a NIL error in
   the degraded case (the paid campaign exists, so failing the job would mislead and be
   unrecoverable by retry — idempotency short-circuits a re-dispatch), and instead makes
   the shortfall VISIBLE via the distinct status (details are in `Result`/Steps) for a
   human or monitor to reconcile. The orchestrator fills project/brief/job/platform
   (and, for a retained ambiguous orphan, a `pending` status).

## The claim contract (release vs retain)

The claim is PERMANENT until released — deliberately NOT auto-reclaimed on a timer. `pending`
is overloaded: it marks both a claim merely in flight AND an AMBIGUOUS dispatch outcome, which
the orchestrator persists as `pending` precisely because the provider MAY already have created
a paid campaign. No column distinguishes the two, so a time-based reclaim would eventually
authorize a DUPLICATE paid create against a campaign that already exists upstream — the exact
failure the claim exists to prevent. Safe automatic recovery needs provider idempotency keys or
an authoritative reconcile first (LFXV2-2665).

The cost is that a pod crashing between claim and release strands a `pending` row that blocks
every future dispatch for that pair, recoverable only by a human. `StuckDispatchClaims` makes
those rows VISIBLE (read-only, `stuckClaimReportAge` = 4m, bounded by `providerCallTimeout` so a
healthy in-flight claim is never reported) instead of leaving them silently invisible until
someone notices a campaign will not dispatch.

**Every stuck claim requires an upstream-platform check before deletion — including a bare
`version = 1` row with no platform id and no result blob.** That shape is NOT evidence the
provider was never called: `dispatchOne` retains the claim WITHOUT upserting when a dispatcher
returns `(nil, nil)`, when it returns an empty upstream id, and when it returns a non-pre-create
`(nil, err)`. In each case a paid campaign may already exist upstream while the row remains
byte-for-byte identical to an abandoned pre-create claim. Confirming that no worker is running
is therefore NOT sufficient to delete: the schema cannot distinguish the two, so the only safe
floor is to check the platform. The `remediation` field on each logged claim states this, and
`version`/`platform_campaign_id`/`has_result` only sharpen WHY the check is owed, never waive it.

The orchestrator single-flight-claims a `(brief, platform)` pair before dispatch and
decides, from the returned error, whether to RELEASE the claim (retry-safe) or RETAIN
it (a blind retry could double-create). Adapters drive that decision:

- A failure that happened BEFORE any upstream create — missing/invalid/undecryptable
  connection, config/validation errors, incomplete credentials, or a client `(nil,
  err)` — is wrapped as a `preCreateError` (via `notCreated`), which implements
  `NoUpstreamCreate() bool`. The orchestrator detects it with `errors.As` and RELEASES
  the claim.
- Any NON-NIL client result returned alongside an error means something may have
  landed upstream, so the adapter hands the campaign back with the error and the
  orchestrator RETAINS the claim. The decision keys on `result == nil` ALONE — NOT on
  whether the campaign id is populated: an ambiguous first-create (or a 2xx with no
  id) returns a non-nil, name-only partial whose `PlatformCampaignID` is EMPTY, and
  that still must retain the claim (LinkedIn even returns a non-nil result carrying a
  `CampaignGroupID` on a definite campaign failure, because the group is permanent).
  The retained row is recorded as a recoverable orphan; its upstream id may be empty
  until reconciled.

## Registration

Adapters are registered in `internal/container` (`registerDispatchers`), called from
BOTH the fast path and the cold-start retry path so the set is identical regardless
of how the DB comes up. A provider without a registered adapter records jobs that
report "no dispatcher registered" (logged as a startup warning via
`logMissingDispatchers`); adapters land incrementally per platform.

The registered set is `registerDispatchers` in `internal/container` — check there for which
providers currently have an adapter rather than duplicating the list here, since it grows
per platform PR and the two drift.

Each adapter interprets its own credential + config shape; see the "Dispatch adapter
(internal/dispatch)" section of the matching platform concept for the per-platform detail:
[reddit](internal-platform-reddit.md), [linkedin](internal-platform-linkedin.md),
[meta](internal-platform-meta.md), [twitter](internal-platform-twitter.md),
[googleads](internal-platform-googleads.md), [microsoft](internal-platform-microsoft.md),
[hubspot](internal-platform-hubspot.md).

## Status toggle (optional capability)

`StatusToggler` is an OPTIONAL dispatcher interface (separate from `PlatformDispatcher`) —
`ToggleStatus(ctx, projectID, platform, campaign *model.Campaign, status)` — for
pausing/resuming a live campaign on the platform (ACTIVE↔PAUSED). It receives the full
persisted `*model.Campaign` (not just the id) so an adapter can reach any child ids it stored
at creation. The orchestrator's `ToggleCampaignStatus` type-asserts it (returning
`ErrToggleUnsupported` when a platform hasn't wired it), so it can be added platform-by-platform
without touching every adapter.

An UNCONFIRMED client outcome (via `<platform>.IsOutcomeUnconfirmed`) is wrapped in
`unconfirmedToggleError`, whose `Unconfirmed()` the service detects across the package
boundary (same behavioral-interface pattern as `NoUpstreamCreate`) — every adapter that
implements `StatusToggler` follows this contract; see the linked platform concepts below
for which do and their implementation details.

Which children a toggle must reach, any asymmetric ACTIVATE/PAUSE handling, and
whether a platform has wired `StatusToggler` at all, is per-platform and
documented in that platform's own "Dispatch adapter (internal/dispatch)"
section: [reddit](internal-platform-reddit.md), [linkedin](internal-platform-linkedin.md),
[meta](internal-platform-meta.md), [twitter](internal-platform-twitter.md),
[googleads](internal-platform-googleads.md), [microsoft](internal-platform-microsoft.md),
[hubspot](internal-platform-hubspot.md) — not summarized here, since a
platform wiring or changing its toggle behavior would otherwise mean editing
this shared file too.

## Metrics read (optional capability)

`MetricsReader` is a second OPTIONAL dispatcher interface, alongside `StatusToggler` —
`ReadMetrics(ctx, projectID, platform, campaign *model.Campaign, window model.MetricsWindow)
(*model.CampaignMetrics, error)` — for a live, read-only performance snapshot of one
campaign (impressions, clicks, cost, CTR) over a caller-supplied window. Same pattern as the
toggle: the orchestrator's `ReadCampaignMetrics` type-asserts it (returning
`ErrMetricsUnsupported` when a platform's dispatcher doesn't implement it, without ever
contacting the platform), so it is added platform-by-platform without touching every adapter,
and it receives the full persisted `*model.Campaign` so an adapter can reach any child ids it
stored at creation (e.g. an ad group/ad set id, if a platform's reporting API needs it).
Unlike `ToggleStatus`, a `ReadMetrics` call has no ambiguous mutation to protect — there is
nothing to leave in an unknown state — so adapter errors propagate to the service verbatim; there
is no UNCONFIRMED wrapping equivalent to `unconfirmedToggleError`.

`window` arrives as a closed, platform-agnostic `model.MetricsWindow` value (`today`,
`yesterday`, `last_7_days`, `last_14_days`, `last_30_days`, `this_month`, `last_month`) — never
a platform's own literal. Each platform adapter owns the mapping from this vocabulary to its
platform's actual query syntax (e.g. Google Ads' GAQL `DURING` literals, Meta's Insights
`date_preset`), and any platform-specific validation of the mapped value (e.g. an allow-list
guard against GAQL injection) belongs in that platform's client package, not in the adapter or
the orchestrator.

**Microsoft Ads is NOT a `MetricsReader` and is not expected to become one under this
contract.** Its Campaign Management API v13 (REST/JSON, synchronous — what the existing
create dispatcher and status toggle use) has no metrics surface. Metrics live in a wholly
separate service, the Reporting API v13: SOAP, and asynchronous
(`SubmitGenerateReport` → poll `PollGenerateReport` until the status leaves `Pending` →
download a zipped CSV via a `ReportDownloadUrl`). There is no synchronous "impressions for
this campaign" call, so it cannot satisfy `ReadMetrics`'s one-bounded-call contract within
`metricsCallTimeout` (20s). Closing this gap needs a design decision (e.g. a bounded
submit-and-poll with a hard ceiling, or a persisted/sweeper-refreshed snapshot instead of a
live read) — deferred, not attempted here.

**X/Twitter** implements it: `twitterMetricsWindow` maps the shared `model.MetricsWindow`
vocabulary to X Ads' own `MetricsWindow` literals, then `GetCampaignMetrics(ctx, campaignID,
window)` queries the X Ads `/stats` endpoint. **CRITICAL: X's stats endpoint caps queryable
date ranges at 7 days per request.** Only `yesterday`, `today`, and `last_7_days` map to a
supported X window (`YESTERDAY`/`TODAY`/`LAST_7_DAYS`); every other foundation window
(`last_14_days`, `last_30_days`, `this_month`, `last_month`) returns `twitter.ErrUnsupportedWindow` explaining
the platform's API limitation (NOT a reduced range, average, or extrapolation). This is a
permanent X API constraint documented in the knowledge base. Spend is returned by X as
`billed_charge_local_micro`, already in micro-currency units (no USD parsing or conversion).

## Account discovery (optional capability)

`AccountLister` is a third OPTIONAL dispatcher interface, alongside `StatusToggler` and
`MetricsReader` — `ListAccounts(ctx, projectID, platform) ([]model.AccessibleAccount, error)` —
enumerating the ad accounts reachable **upstream at the provider** with the connection's stored
credential. It exists so an operator configuring a connection can pick the right account instead
of pasting a customer ID by hand. Same shape as the other two: the orchestrator's `ReadAccounts`
type-asserts it and returns `ErrAccountsUnsupported` when a platform's dispatcher does not
implement it, without contacting the platform.

Note what it is NOT: this does not list anything this service stores. A project holds at most one
connection per provider, and that singleton is read via `GET .../connection-{provider}`.
`AccessibleAccount` (`ID`, `Label`) is a live projection of the provider's own account list, never
persisted — the same live-read-only discipline as `ReadMetrics`. Errors propagate verbatim (a read
has no ambiguous mutation to protect), surfacing as 400 for an unsupported platform and 503 for a
provider failure.

Google Ads is the only implementation today, via
`Client.ListAccessibleCustomers` → `customers:listAccessibleCustomers`. That endpoint is
**account-agnostic** — it has no `customers/{id}` path segment, unlike every other Google Ads call
— but it still goes through the shared `doRequest` helper, with a nil body (so no `Content-Type`
is sent) and `idempotent=true` (a pure read, so retrying a 429 cannot double-apply anything).
Routing it through `doRequest` rather than hand-rolling the transport keeps one copy of the URL
construction, body bounding, and `apiError`/`transportError` classification.

## Channel kinds: paid ads vs email

`model.ChannelKind` classifies each provider as **`paid-ads`** or **`email`** (`Provider.Kind()`,
with `Provider.IsPaidAds()` as the common shorthand). The distinction is BEHAVIOURAL, not
cosmetic: a paid ad channel CREATES a campaign that spends budget and can be paused/resumed
mid-flight, whereas the email channel STAGES a draft a human sends — no budget, no delivery
this service controls, nothing to pause.

HubSpot is the only email provider today. Branch on `Kind()` rather than comparing against
`ProviderHubSpot`, so a second email provider does not require hunting down every hardcoded
check. `Kind()` enumerates providers explicitly and returns `""` for an unclassified one, so a
newly added provider surfaces the omission instead of silently inheriting paid-ads behaviour.

Two places this shows up today:

- `dispatchableProviders` (container) spans BOTH kinds — email is dispatchable even though it
  is not an ad platform — which is why it is named for dispatch rather than "ad platforms".
  `logMissingDispatchers` logs each missing provider's kind so an operator can tell a missing
  paid platform (budget unspent) from a missing email channel (no drafts staged).
- The `ErrToggleUnsupported` 400 distinguishes the two reasons: email has no run state BY
  DESIGN, while an ad platform's toggle may simply not be wired yet.

See [internal/dispatch](../../../internal/dispatch).
