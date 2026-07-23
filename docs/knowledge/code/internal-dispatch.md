---
type: "Go Package"
title: "internal/dispatch"
description: "Per-platform PlatformDispatcher adapters bridging the orchestrator to the ad-platform API clients."
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

Registered so far (`registerDispatchers`): **reddit**, **linkedin**, **meta**,
**twitter** (the OAuth1 4-tuple adapter, LFXV2-2642), **googleads** (LFXV2-2643). The
email (HubSpot) dispatcher is LFXV2-2777; Microsoft Ads is LFXV2-2804.

Each adapter interprets its own credential + config shape:
- **reddit** — OAuth2 (clientId/secret/refreshToken); AccountID from the connection.
- **linkedin** — a single OAuth2 accessToken; builds RuntimeConfig from the
  connection's AccountID + `org_id` (must be the NUMERIC org id) plus caller-supplied
  targeting profiles from config.
- **meta** — a single OAuth2 accessToken; AccountConfig from AccountID (`act_...`) +
  `page_id` (REQUIRED by the connection design — the dispatcher needs it to attach the
  promoted-object page, so requiring it at connection time surfaces a 4xx instead of a
  silent dispatch failure); Budget is in the ACCOUNT's currency (no FX), optional
  CurrencyOffset.
- **twitter** — OAuth1 4-tuple (consumer key/secret + access token/secret); AccountConfig
  from AccountID + `funding_instrument_id`. Budget (`budgetAmount`) is in the ACCOUNT's
  currency (no FX). Surfaces a `Reused` reuse/config-drift flag and classifies an
  exhausted mutating 429 as UNCONFIRMED; validates the destination URL (https/http, no
  embedded userinfo) up front.
- **googleads** — OAuth2 application (clientId/secret + refreshToken) PLUS a Google Ads
  API developer token; AccountConfig from AccountID (the customer id) + an OPTIONAL
  `login_customer_id` (the manager/MCC account, from the connection's ProviderConfig).
  Budget (`googleAdsConfig.budget`) is in the ACCOUNT's currency (no FX). The client
  today creates a PAUSED search-campaign shell (budget → campaign); its two-step
  hierarchy means an orphaned budget is reconciled by its own deterministic
  `CampaignBudgetName`, so the dispatcher returns a non-nil name-only result (retaining
  the claim) on an ambiguous/duplicate-name create rather than releasing on an empty id.
