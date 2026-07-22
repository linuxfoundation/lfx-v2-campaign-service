<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Plan — Dispatch Claim Resume / Reconciliation (LFXV2-2665)

## Problem

The orchestrator single-flight-claims each `(brief, platform)` pair with an atomic
`INSERT ... ON CONFLICT (brief_id, platform) DO NOTHING` of a `pending` campaign row
(`ClaimCampaignDispatch`, `internal/infrastructure/postgres/campaign_repo.go:41`). This
correctly prevents two concurrent workers from double-creating an upstream campaign.

But a `pending` claim is **terminal**: once a dispatch fails after a partial upstream
create (e.g. LinkedIn created the campaign GROUP but not the campaign, or any platform's
ambiguous first-step failure), the row is RETAINED as `pending` (correctly — a blind
retry could duplicate the paid resource). #42 made the partial's `Result` **recorded**,
so the orphan is now discoverable. What is still missing:

- **No re-dispatch path.** A later job for the same pair hits
  `ON CONFLICT DO NOTHING` → `claimed=false` → the `!claimed` branch marks the platform
  `Skipped` (`orchestrator.go:508-529`) and never invokes the dispatcher. So the
  platform clients' name-based find-or-create resume (e.g. LinkedIn
  `findOrCreateCampaignGroup`, `internal/platform/linkedin/client.go:1019`) never runs.
- **Nothing reaps a stuck pending claim.** The staleness sweeper operates on *jobs*
  (`FailStuckJobs`), not on `campaigns` claim rows. A pending claim blocks the pair
  **forever** until a human deletes the row.

Net: after a partial failure the pair is permanently stuck — recorded (post-#42) but
unrecoverable without manual DB intervention.

## Goal

Let a **new, explicit** dispatch attempt for a pair that is currently `pending`
(orphaned, not actively owned) safely re-dispatch, so the client's idempotent
find-or-create resumes the partial. Do this WITHOUT reintroducing the double-create risk
the claim exists to prevent, and WITHOUT auto-retrying inside the same job run.

## Design

### Core idea: a claim has an *owner lease*, and a stale lease is re-claimable

Today "pending" conflates two states: (a) **actively owned** — a worker is mid-dispatch
right now; and (b) **orphaned** — a prior dispatch died/failed and nothing owns it. A
blind retry is only dangerous for (a). We distinguish them with a **lease timestamp** and
only re-claim rows whose lease has expired (i.e. genuinely orphaned).

### D1. Schema (migration `000004`)

`campaigns.status` has **no CHECK constraint** (verified in `000002`), so no constraint
migration is needed. Add one column:

```sql
ALTER TABLE campaigns ADD COLUMN claimed_at TIMESTAMPTZ;
```

- Set `claimed_at = now()` when a claim is taken or re-taken.
- `updated_at` already bumps on every upsert; `claimed_at` specifically tracks *lease
  acquisition* so a long-running-but-alive dispatch isn't wrongly stolen mid-flight
  (its `claimed_at` is recent because it just acquired; the lease window must exceed
  `providerCallTimeout`).

Backfill: `claimed_at` defaults NULL; a NULL `claimed_at` on an existing `pending` row is
treated as "lease unknown / older than any window" → re-claimable (safe: those predate
this feature and are known-stuck).

### D2. `ClaimCampaignDispatch` becomes claim-or-steal-if-stale

Replace the plain `DO NOTHING` with a conditional re-claim. One atomic statement:

```sql
INSERT INTO campaigns (project_id, brief_id, job_id, platform, campaign_name, status, claimed_at)
VALUES ($1, $2, $3, $4, '', 'pending', now())
ON CONFLICT (brief_id, platform) DO UPDATE
  SET job_id = EXCLUDED.job_id, claimed_at = now(), version = campaigns.version + 1
  WHERE campaigns.platform_campaign_id = ''              -- never steal a completed campaign
    AND campaigns.status = 'pending'                     -- only a pending claim is re-claimable
    AND (campaigns.claimed_at IS NULL
         OR campaigns.claimed_at < now() - $5::interval) -- lease expired → orphaned
RETURNING (xmax = 0) AS inserted;                        -- xmax=0 ⇒ fresh INSERT, else UPDATE
```

- `$5` = **lease TTL** (`claimReclaimAfter`, see D4). Must be `> providerCallTimeout` so an
  in-flight dispatch is never stolen.
- The `WHERE campaigns.platform_campaign_id = ''` guard makes stealing a *completed*
  campaign impossible even if statuses drift — the id is the hard invariant.
- If the row conflicts but the `WHERE` fails (actively owned, or already completed), the
  `DO UPDATE` affects 0 rows → the statement returns no row → `claimed=false`,
  `RowsAffected()==0`. Caller reads the existing row and takes the current `!claimed`
  path (reuse-if-completed / skip-if-owned) unchanged.
- `RETURNING (xmax = 0)` distinguishes a fresh insert from a steal so the orchestrator can
  log/emit a "resumed a stale claim" signal.

This keeps single-flight intact (only ONE worker wins the steal, arbitrated by the row
lock on the conflicting row) while making an orphaned lease recoverable.

### D3. Orchestrator: on a successful (re)claim of a pending orphan, dispatch normally

`dispatchPlatform` already dispatches when `claimed==true`. With D2, a steal returns
`claimed==true`, so it flows into the existing dispatch path **unchanged** — the client's
find-or-create resumes (finds the orphaned group by name, creates the campaign). The
partial-persist logic from #42 records the (now hopefully more complete) result.

One addition: when the (re)claimed row already carried a `Result` (a prior orphan), pass
it to the dispatcher OR at minimum preserve it if the resume fails again, so we don't lose
the earlier orphan detail. Simplest: the client is idempotent by name and doesn't need the
prior Result; the orchestrator just overwrites on the next upsert. (Confirm per-platform:
LinkedIn resumes purely by name, so prior Result is not needed as input.)

### D4. Lease TTL constant

```go
// claimReclaimAfter is how long a pending claim's lease must be idle before another
// dispatch may steal it. MUST exceed providerCallTimeout so an in-flight dispatch is
// never stolen mid-create. Generous to avoid racing a slow-but-alive provider.
const claimReclaimAfter = 3 * providerCallTimeout
```

`providerCallTimeout` is the existing per-call ceiling; `3×` gives comfortable margin
against clock skew across replicas and a dispatch that is retried internally.

### D4b. Resume is PER-PLATFORM gated (critical — clients differ)

Audit of the merged/stacked clients shows find-or-create idempotency is **not uniform**,
so a blanket steal-and-redispatch would double-create on the non-idempotent platforms:

| Platform | Resume mechanism | Safe to steal-and-redispatch? |
|---|---|---|
| **LinkedIn** | `findOrCreateCampaignGroup` — name-based find-or-create (`client.go:1019`) | ✅ yes |
| **Twitter/X** | `findByName` name-based idempotency on campaign + line item (`client.go:929/946`) | ✅ yes |
| **Reddit** | Comment states it makes the orphan identifiable but **does NOT resume creation** — a re-dispatch would create a second campaign | ❌ no (would duplicate) |
| **Meta** | No name-based find-by-name found; blind create | ❌ no (would duplicate) |
| **Google** | (audit before enabling) | ⚠️ verify |

Therefore the steal must be **gated by a per-dispatcher capability**, not applied globally:

```go
// Resumable reports whether re-dispatching a pending (brief, platform) orphan is
// safe — i.e. the client find-or-creates by name so a resume reuses the partial
// instead of duplicating it. A non-resumable platform's stale claim is left for
// manual/other reconciliation, never auto-stolen.
type ResumableDispatcher interface { Resumable() bool }
```

The lease-steal in `ClaimCampaignDispatch` (D2) only fires when the resolved dispatcher
implements `Resumable() == true`. A non-resumable platform keeps today's behavior (stale
pending claim is NOT stolen; it stays for manual reconciliation). Enable LinkedIn +
Twitter first; add reddit/meta/google as their clients gain name-idempotent resume.

### D5. Re-entry trigger

Resume is driven by a **new job** for the pair (a user re-submitting CreateCampaigns, or
a future reconcile sweep). This PR does NOT add an automatic background re-dispatcher —
keeping the trigger explicit (a new job) avoids a runaway retry loop and matches the
existing "jobs are the unit of work" model. A background reconcile sweep can be layered on
later (out of scope; note as a follow-up).

## What we deliberately do NOT do

- No CHECK-constraint migration (status is unconstrained; adding `group_created` etc. is a
  separate ergonomics change flagged in #42's dealako review).
- No automatic in-run retry (a single job still attempts each platform once).
- No background reconcile daemon (explicit re-submission only, this iteration).

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| Stealing an in-flight dispatch → double-create | Lease TTL `> providerCallTimeout`; steal only when `claimed_at` is stale AND `platform_campaign_id=''`. |
| Two replicas steal the same stale claim simultaneously | The `ON CONFLICT DO UPDATE` row lock serializes them — exactly one wins, the other gets `claimed=false`. Same guarantee as today's insert. |
| A completed campaign gets stolen | `WHERE platform_campaign_id = ''` — a completed row (non-empty id) is never matched. |
| Client resume creates a duplicate on a non-idempotent platform | **Per-platform `Resumable()` gate (D4b)** — steal only fires for platforms whose client find-or-creates by name (LinkedIn, Twitter confirmed). Reddit + Meta are NOT resumable today and are explicitly excluded until their clients gain name-idempotent resume. |

## Test plan

- `ClaimCampaignDispatch` unit tests: (a) fresh insert; (b) steal a stale pending orphan
  (claimed_at old / NULL) → `claimed=true`, `inserted=false`; (c) do NOT steal a fresh
  pending lease (claimed_at recent) → `claimed=false`; (d) do NOT steal a completed row
  (non-empty id) → `claimed=false`; (e) concurrent steal → exactly one winner.
- Orchestrator: a second job for a stale group-orphan pair re-dispatches (dispatcher
  invoked), and on success the row flips to a completed campaign (non-empty id, no longer
  `pending`).
- Regression: an actively-owned pending claim (recent lease) still skips (no double
  dispatch) — the existing single-flight test must still pass.

## Rollout / sequencing

1. Depends on: #42 (orphan is recorded) merged, and the dispatcher stack merged (so the
   clients' find-or-create resume paths exist on main).
2. Migration `000004` (additive column, migrate-on-boot safe).
3. `ClaimCampaignDispatch` change + `claimReclaimAfter` + tests.
4. Per-platform confirmation that find-or-create is name-idempotent before relying on
   resume (LinkedIn confirmed; audit reddit/meta/twitter/google).

## Estimated size

Small-to-medium: 1 additive migration, ~1 repo method rewrite, ~1 constant, ~6 tests. No
API surface change. The bulk of the risk is the SQL correctness + the per-platform
find-or-create audit.
