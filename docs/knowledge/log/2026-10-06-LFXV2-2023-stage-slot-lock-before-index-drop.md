# 2026-10-06 — Slot versions: stage the per-slot lock one release before the index drop

**Update** — PR #260 was split. Shipping the drop of `000022`'s three-column
`uq_campaigns_brief_platform_variant_live` in the same release as the per-slot adopt/claim
advisory lock was not safe for an image-only rollback (PR review): reverting the image does not
revert the schema, and the #247 binary takes no slot lock, so with the index gone its adopt
preflight could race a slot-2 claim and bind slot 1 beside it.

**What this release ships.** Only the lock: `ClaimCampaignDispatch` (a short transaction),
`UpsertCampaign` and `AdoptCampaign` take `pg_advisory_xact_lock` keyed by
`(brief_id, platform, variant)`, the adopt takes it before the brief's `FOR UPDATE` and checks
for any live row on the slot under it. With the legacy index still in place the lock is
redundant with it for correctness, which is what lets it ship alone.

**What it no longer ships.** The drop-index migration (it was `000038`, renumbered to `000040`
by the merge with `main` — which now owns `000038` keyword insight reports and `000039`
`max_cpc_bid` — and then removed), the `requiredIndexes` change, and the retirement of
`domain.ErrSlotVersionUnavailable`, the orchestrator's "not available yet" branch, the claim's
and adopt's legacy-index `23505` handling and the expand-phase orchestrator test: all of those
depend on the index being gone and were restored to `main`'s form. The `new_version` API
description again says the request is refused until the index is dropped. Parts of the
2026-10-05 slot-versions contract entry that describe the drop and those removals no longer
apply to this release.

**When the index goes.** One release after this lock is deployed, in its own PR: the contract
migration (next free number at that time), dropping the entry from `requiredIndexes`, retiring
`ErrSlotVersionUnavailable`, and the contracted-schema live tests (slot 2 created beside slot 1;
slots 3–8 claimed concurrently, each once). By then every binary that can run against the
contracted schema, including the one a rollback returns to, takes the lock; rolling back THIS
release leaves the index in place.

**Tests.** Live tests now run against the schema this release ships (legacy index present):
`TestLiveSlotVersionDuringExpandPhase` is back; `TestLiveConcurrentNewVersionClaimsHaveOneWinner`
races slot-2 claims on a slot whose slot 1 was deleted and catches a missing lock
race-dependently (with the claim's lock removed it failed one run in three: a loser hit the
legacy index and returned `ErrSlotVersionUnavailable`);
`TestLiveClaimAndAdoptWaitForTheSlotLock` now parks a slot-2 claim (expects the committed row back,
not claimed) instead of slot 3; `TestLiveAdoptRefusesASlotWhoseOnlyLiveCampaignIsALaterVersion`
deletes slot 1 before claiming slot 2. Concepts updated: `code/internal-infrastructure-postgres.md`,
`code/internal-service.md`, `kubernetes/deployment.md`.
