---
type: "Go Package"
title: "internal/dispatch"
description: "Per-platform PlatformDispatcher adapters bridging the orchestrator to the channel API clients (six paid ad platforms plus the hubspot email channel)."
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
boundary (same behavioral-interface pattern as `NoUpstreamCreate`) — every adapter's
`ToggleStatus` below follows this contract.

Which children a toggle must reach, any asymmetric ACTIVATE/PAUSE handling, and
whether a platform has wired `StatusToggler` at all, is per-platform and
documented in that platform's own "Dispatch adapter (internal/dispatch)"
section: [reddit](internal-platform-reddit.md), [linkedin](internal-platform-linkedin.md),
[meta](internal-platform-meta.md), [twitter](internal-platform-twitter.md),
[googleads](internal-platform-googleads.md), [microsoft](internal-platform-microsoft.md),
[hubspot](internal-platform-hubspot.md) — not summarized here, since a
platform wiring or changing its toggle behavior would otherwise mean editing
this shared file too.

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
