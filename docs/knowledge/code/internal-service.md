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

`BriefService` implements brief CRUD and campaign endpoints. `FindBrief` looks a brief up by
`(project_id, event_slug)` rather than by id — the key a caller holds when re-visiting an event
page — returning `ErrNotFound` when the event has no brief yet. That 404 is an ordinary
outcome (first-time generation), not a failure. It never generates or mutates: regeneration is
an explicit `UpdateBrief`, so edits to the AI-generated copy are never silently overwritten. Campaign creation
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
alone is not sufficient, since a partial/degraded campaign can carry an upstream id).
Write ownership of the row is claimed via `CampaignRepo.ClaimCampaignVersion` BEFORE the
paid platform call, not by comparing the read-time version in memory (LFXV2-2901): an
in-memory comparison only rejects a stale caller, it does nothing to stop a SECOND
caller that read the same version from also passing its own check and also calling
the platform, so two toggles — or a toggle and `UpdateCampaign` — could both mutate
the platform before either persisted.

**The claim is a LOCK, not a version bump.** `ClaimCampaignVersion` takes a Postgres session
advisory lock (keyed by campaign id, on a dedicated pooled connection), reads the row at
`expectedVersion`, and leaves the version UNCHANGED. The increment happens later, in
`ReplaceCampaign`, inside the same transaction that writes the outbox event — which is what
preserves the invariant that every campaign write co-commits its index event. An earlier
design bumped at claim time (`UPDATE ... SET version=version+1 WHERE version=$expected`) and
could not hold that invariant; do not reintroduce it. Because ownership is the lock and not
the version, it survives the caller's external I/O: a second writer BLOCKS on the lock
instead of racing between the claim and the post-platform persist. Callers MUST release via
`ReleaseCampaignLock` (deferred), and the not-found vs. stale-version classification is made
while the lock is still held — releasing first would let a concurrent delete turn a
stale-version caller's 412 into a 404. `UpdateCampaign`, which has no I/O between its read
and its write, claims first for the same reason — without it, its single-statement
`ReplaceCampaign` could win the row out from under an in-flight toggle's claim,
losing the toggle's own post-platform persist even though each write was
individually consistent. Both handlers validate BEFORE claiming, so a request that will be
rejected anyway never takes the lock and never blocks legitimate writers. A stale
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

## Campaign metrics read

`BriefService.GetCampaignMetrics` (backing `GET .../campaigns/{id}/metrics`) reads live
performance metrics (impressions, clicks, cost, CTR) directly from the campaign's ad
platform. Unlike `GetCampaign`, this is a pure read — `model.CampaignMetrics` is never
persisted, so there is no `If-Match`/version to check, mirroring `ToggleCampaignStatus`'s
always-live-not-cached semantics. `Orchestrator.ReadCampaignMetrics` type-asserts the
platform's dispatcher for the optional `MetricsReader` capability at call time (the same
optional-capability pattern as `StatusToggler`) rather than requiring every dispatcher to
implement it; a dispatcher that isn't a `MetricsReader` — or a platform with no dispatcher
registered at all — returns `ErrMetricsUnsupported` (400) without ever contacting the
platform. An unprovisioned campaign (`PlatformCampaignID` empty, or `campaign == nil`)
returns `ErrCampaignNotProvisioned` (409) before any platform call, same as the toggle. Any
other error from the dispatcher's `ReadMetrics` call propagates as-is (503) — a read has no
ambiguous mutation to protect, so there is no UNCONFIRMED classification here. The call is
bounded by `metricsCallTimeout` (20s, distinct from `toggleCallTimeout`'s 45s — reads should
fail fast rather than hold a request open).

The `window` query parameter is a closed, platform-agnostic vocabulary
(`model.MetricsWindow`: `today`, `yesterday`, `last_7_days`, `last_14_days`,
`last_30_days` [default], `this_month`, `last_month`) — never a platform's own dialect
(e.g. Google Ads' GAQL `DURING` literals, Meta's Insights `date_preset`). Each platform's
`MetricsReader` adapter is responsible for mapping this vocabulary to its own platform's
query syntax; the mapping (and any platform-specific validation, e.g. an allow-list guard
against GAQL injection) lives in the platform client package, not here.

## Campaign delete

`BriefService.DeleteCampaign` (backing `DELETE .../campaigns/{id}`, `If-Match` required)
SOFT-deletes a campaign locally. Its purpose is slot recovery: the `(brief_id, platform)`
uniqueness that makes dispatch idempotent also meant a campaign row occupied its brief's
slot for that platform permanently, so a wrong-budget campaign or an ambiguously-failed
upstream create blocked the pair forever. The partial unique index from `000010` excludes
deleted rows, so deleting frees the slot for a legitimate re-dispatch.

**It never touches the ad platform, by design.** No provider client in
`internal/platform/*` implements a campaign delete/remove call, and inventing an
unverified one is worse than not offering it: removing the local row while a real paid
campaign keeps spending is the worst available outcome. The API description says so
explicitly — a campaign already created upstream keeps running until it is stopped there
(pause it via the status toggle first). That is also why the delete is SOFT: the retained
row holds `platform_campaign_id`, the only local pointer to whatever may still exist
upstream, so a hard delete would free the slot but destroy the sole record needed to find
and stop the campaign still spending.

Guards, evaluated under a `SELECT … FOR UPDATE` row lock in one transaction: absent or
already-deleted → `ErrNotFound` (404, matching `GetCampaign`, which hides deleted rows);
mid-dispatch `pending` → `ErrConflict`, re-mapped by the service to an ACTIONABLE 409
("being dispatched … wait and retry") rather than `mapBriefErr`'s generic "already
exists"; then the version check → 412. The version check runs LAST so a stale ETag on a
dispatching campaign reports the dispatch, not a misleading "reload and retry". The row
lock is required, not decorative: a `pending` row is an active dispatch claim, and under
READ COMMITTED a plain guarded `UPDATE` cannot see a claim that commits just before the
statement (and the claim INSERTs rather than updates, so there is no row conflict to
serialize on) — deleting under an in-flight dispatch could let a concurrent claim
double-create upstream.

See [internal/service](../../../internal/service).
