# 2026-10-05 — Slot versions, contract step: drop the legacy slot index, lock adopt/claim per slot

**Update** — The expand step (`000036`/`000037`) left `000022`'s three-column
`uq_campaigns_brief_platform_variant_live` in place, so a `new_version` claim on an occupied
slot raised `23505` and was refused as `domain.ErrSlotVersionUnavailable` ("not available
yet"). Migration `000038` drops it (`DROP INDEX CONCURRENTLY IF EXISTS`, alone in its file);
its down re-creates it exactly as `000022` did and fails, correctly, once a slot holds two live
campaigns. `requiredIndexes` lost the entry — leaving it would fail every boot after `000038`.
The four-column `uq_campaigns_brief_platform_variant_slot_version_live` is now the only slot
index, and a second Microsoft campaign on one brief is actually created as `slot_version` 2.

**Why a lock had to come with it.** The dropped index was also what kept an adopt off a slot
that already had a campaign at ANY version. Adopt always writes `slot_version` 1, so after the
drop its `ON CONFLICT` arm misses a live version 2 (version 1 deleted), and an adopt racing a
`new_version` claim conflicts on no index at all — two live rows for one logical slot, one of
them bound by an adopt whose contract is "the slot was empty". `ClaimCampaignDispatch` (now a
short transaction), `UpsertCampaign` and `AdoptCampaign` take a transaction-scoped advisory lock
keyed by `(brief_id, platform, variant)` — two-int form, its own namespace, brief id
canonicalized through `::uuid::text` — and the adopt checks for any live row on the slot under
it, answering `ErrConflict` (409). Adopt takes the slot lock before the brief's `FOR UPDATE`;
in the other order the claim's FK `KEY SHARE` on the brief and the adopt's wait on the lock
deadlock (seen live as `40P01` with the adopt's lock removed).

**Removed as dead.** `domain.ErrSlotVersionUnavailable`, the orchestrator's "not available
yet" branch, the claim's legacy-index `23505` handling (slot-1 lost race and slot-2 refusal),
the adopt's legacy-index classification, the fake repo's `legacySlotIndex` switch and
`TestOrchestrator_NewVersionIsRefusedDuringTheExpandPhase`. Slot-1 claims write the same row as
before. The `new_version` API description and `docs/api-catalog.md` no longer promise a
refusal.

**Tests.** `TestLiveSlotVersionDuringExpandPhase` became `TestLiveNewVersionCreatesSlotVersion2`;
new live tests `TestLiveConcurrentNewVersionClaims` (same-version race: one winner, no error;
distinct versions: all succeed), `TestLiveConcurrentClaimAndAdoptLeaveOneLiveRow`,
`TestLiveAdoptRefusesASlotWhoseOnlyLiveCampaignIsALaterVersion` and
`TestLiveClaimAndAdoptWaitForTheSlotLock` (deterministic: holds the lock with an uncommitted
slot-2 row and asserts both adopt and claim wait). The dispatch-index live/unit tests and the
hand-written `ON CONFLICT` statements in `schema_live_test.go` / `campaign_variant_live_test.go`
moved to the four-column target.
