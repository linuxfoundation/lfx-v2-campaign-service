---
type: "Go Package"
title: "internal/service"
description: "Campaign service business logic, including Readyz (DB-backed readiness) and Livez (process-only liveness)."
resource: "internal/service"
---

# internal/service

Package service contains the campaign service business logic, including the
implementation of the generated Goa service interfaces.

`GET /readyz` ANDs an optional `ReadinessChecker` (the PostgreSQL pool when
wired) into readiness with a ~2s timeout and returns `503` when the dependency
is unhealthy. `GET /livez` remains process-only so database outages do not
trigger Kubernetes restarts.

`AudienceService` implements the built-audience endpoints (the "B2" resource,
epic LFXV2-2770): create/get/list/update of a `campaign_audiences` row subordinate
to a brief. An audience is a POINTER + provenance to a platform-side audience (a
HubSpot master contact list), NOT its contents. Update is optimistic-concurrency
gated on `If-Match` (same strong-validator parsing as briefs); the ETag mirrors the
row version. Like the other services it late-binds via `SetBackend` after a
cold-start DB retry and returns a typed `503` (routes mounted) when no repo is wired.

`BriefService` implements brief CRUD and campaign endpoints. Campaign creation
(`CreateCampaigns`) requires an approved brief, rejects empty and duplicate
platform sets (a duplicate would create two paid upstream campaigns), then hands
off to the `Orchestrator`, which persists a job and dispatches per platform
asynchronously (bounded concurrency). Dispatch is idempotent: a brief already
carrying a COMPLETED campaign for a platform is reused rather than re-created. The
idempotency fast-path lookup (`GetCampaignByPlatform`) distinguishes its outcomes: an
existing campaign with an upstream id AND a terminal status (`created` /
`created_degraded`) short-circuits to reuse; a `pending` row — even one carrying an
upstream id or a Result reconcile blob — is a retained partial ORPHAN, not a
completed campaign, so it does NOT short-circuit (on retry it is reported as
reconciliation-required rather than a false success); `ErrNotFound` (no row yet) falls
through to `ClaimCampaignDispatch`; but a REAL DB error (anything else) is surfaced as
a platform failure (logged at ERROR) rather than silently treated like "no existing
campaign" — proceeding to
claim/dispatch when an existing campaign merely couldn't be loaded could duplicate an
upstream create, so it fails loud instead. Replacing a brief's
content resets it to `draft` (re-approval required). Optimistic concurrency is enforced via
version/If-Match (`428` when missing, `412` on mismatch).

Dispatch is durable (LFXV2-2665): single-flight per (brief, platform) is
enforced by an atomic claim — `ClaimCampaignDispatch` does INSERT ... ON CONFLICT
DO NOTHING of a `pending` campaign row, so exactly one worker across replicas
wins the claim (the unique index arbitrates) with no held connection or blocking
lock. A worker that loses the claim reuses the existing row instead of dispatching
again; the pending row also survives an upstream-create-then-crash, making the
orphaned upstream campaign recoverable. The orchestrator tracks in-flight runs
and its `Shutdown` drains them (bounded) before the DB pool closes, and on
startup jobs left non-terminal beyond a staleness cutoff are failed-forward (they
cannot be safely resumed without provider idempotency keys).

## Campaign status toggle

`BriefService.ToggleCampaignStatus` (backing `PATCH .../campaigns/{id}/status`
{active|paused}) pauses/resumes a campaign ON THE PLATFORM, then persists. Unlike
`UpdateCampaign` (DB-only), the platform call happens FIRST via
`Orchestrator.ToggleCampaignStatus` → the platform's `StatusToggler`; the DB row is written
only after the platform confirms. Only a fully-created campaign (`created`, or one already `active`/`paused`) may be toggled
(`model.CampaignStatusToggleable`); a `pending` ambiguous orphan or a `created_degraded`
campaign is rejected 409 — toggling one would activate an incomplete campaign and/or
overwrite its reconciliation marker with the run state (a non-empty `PlatformCampaignID`
alone is not sufficient, since a partial/degraded campaign can carry an upstream id). A stale
`If-Match` fails BEFORE the paid platform call;
failures are classified (`ErrCampaignNotProvisioned` → 409 — a campaign with no upstream id yet,
OR one that on ACTIVATE lacks the child ad group/ad ids needed to serve, so not every 409 means
an unfinished create; `ErrToggleUnsupported` → 400, an UNCONFIRMED outcome → 503 "verify before
retrying", a definite platform failure → 503 "not modified") rather than all blamed on the
platform. An
UNCONFIRMED outcome is a transport/5xx/redirect error the PATCH may have applied — the client
exposes it via `reddit.IsOutcomeUnconfirmed`, the dispatcher wraps it in an error whose
`Unconfirmed()` reports true (same behavioral-interface pattern as `NoUpstreamCreate`), and
the handler surfaces it without lying either way and without writing the row. The post-platform
`ReplaceCampaign` runs on a `context.WithoutCancel` context BOUNDED by `persistResultTimeout`,
so the row can't diverge from the platform if the request is cancelled after the PATCH commits
and a stuck DB can't hang shutdown; a persist failure after the platform changed is logged as a
divergence reconcile signal.

## Connection credential verification (three-state)

`testConn` (shared by all seven per-provider `Test*` handlers) backs
`POST /projects/{projectId}/connection-{provider}/test`. It returns an AUTHORITATIVE
`state` — `verified` / `invalid` / `unverifiable` — with `ok` DERIVED from it
(`ok == state=="verified"`) and retained only for wire compatibility.

Previously this reported `ok = c.HasCredentials()`: credential PRESENCE, not validity. A
stored-but-expired token answered `true`, and the failure surfaced only later as a dispatch
failure on a paid campaign. (The old `message` was honest about being a stub; the defect was
that `ok` is the field callers branch on.)

The state is resolved in order: no stored credential → `unverifiable` (nothing to verify —
distinct from "rejected", which is a different fix); no verifier wired for the provider →
`unverifiable`; otherwise the provider is probed under `verifyCallTimeout` (20s, well under the
60s HTTP write timeout, so a hung provider cannot outlive the response) and the dispatcher's
verdict is returned. `testResult` FAILS CLOSED on an unrecognized state, reporting
`unverifiable` rather than defaulting to a verdict in either direction.

The handler NEVER writes the connection row. In particular an `unverifiable` result does not
set `status = error`: a transient provider outage must not durably brand every project's
working connection as broken.

The verifier map is injected by the container via `SetVerifiers`, alongside `SetBackend`, on
BOTH the fast path and the cold-start retry path — so a cold-started pod's `test` endpoint
behaves identically to a warm one.

See [internal/service](../../../internal/service).
