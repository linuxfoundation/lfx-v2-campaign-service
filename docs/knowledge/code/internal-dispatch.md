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
2. **Map inputs** (per-platform) — the adapter reads the brief's shared event fields
   (eventName / registrationUrl / project from the brief's opaque JSON blobs) and the
   per-platform `config json.RawMessage` (budget, dates, objective, targeting — the
   caller's `CreateCampaigns` `Input.Config`) onto the client's `CampaignInput`.
3. **Call the client** and map the result → `model.Campaign` (upstream id, name, and
   the provider result blob in `Result`). The orchestrator fills
   project/brief/job/platform/status.

## The claim contract (release vs retain)

The orchestrator single-flight-claims a `(brief, platform)` pair before dispatch and
decides, from the returned error, whether to RELEASE the claim (retry-safe) or RETAIN
it (a blind retry could double-create). Adapters drive that decision:

- A failure that happened BEFORE any upstream create — missing/invalid/undecryptable
  connection, config/validation errors, incomplete credentials — is wrapped as a
  `preCreateError` (via `notCreated`), which implements `NoUpstreamCreate() bool`. The
  orchestrator detects it with `errors.As` and RELEASES the claim.
- A client that returns `(nil, err)` means nothing was (or may have been) created —
  the adapter wraps it `notCreated` too.
- A client that returns a non-nil partial result alongside an error (the create is
  ambiguous / may have landed) is handed back with the upstream id populated and a
  non-nil error, so the orchestrator RETAINS the claim and records the recoverable
  orphan.

## Registration

Adapters are registered in `internal/container` (`registerDispatchers`), called from
BOTH the fast path and the cold-start retry path so the set is identical regardless
of how the DB comes up. A provider without a registered adapter records jobs that
report "no dispatcher registered" (logged as a startup warning via
`logMissingDispatchers`); adapters land incrementally per platform.

Registered so far: **reddit** (`RedditDispatcher`). Google/LinkedIn/Meta/Twitter and
the email (HubSpot) dispatcher follow.

See [internal/dispatch](../../../internal/dispatch).
