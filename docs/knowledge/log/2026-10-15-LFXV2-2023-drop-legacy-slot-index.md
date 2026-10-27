# 2026-10-15 — Slot versions, contract step: drop the legacy slot index

**Update** — Step 3 of the LFXV2-2023 rollout, stacked on #260 and to be merged only one release
after #260 is deployed. Migration `000050` drops `000022`'s three-column
`uq_campaigns_brief_platform_variant_live` with `DROP INDEX CONCURRENTLY IF EXISTS` (alone in its
file); its down re-creates the index with `000022`'s name, keys and predicate (no `IF NOT EXISTS`,
for `000024`'s down reason) and fails, correctly, once a slot holds two live campaigns. The
four-column `uq_campaigns_brief_platform_variant_slot_version_live` is now the only slot index,
so a `new_version` claim for slot 2 succeeds beside slot 1 instead of being refused.

**Removed.** The legacy entry in `requiredIndexes`, and with it the `optional` flag and
`RequiredIndexIsOptional` (it was the only optional entry); `domain.ErrSlotVersionUnavailable`
and the orchestrator's "not available yet" branch; every legacy-index `23505` classification —
the claim's (refusal above slot 1, lost race at slot 1), the adopt's (`ErrConflict`) and the
upsert's INSERT-arm lost claim; the fake repo's `legacySlotIndex` switch and its expand-phase
test; `contractedSchemaRepo` (the shared test database is now the contracted schema). Kept:
the per-slot advisory lock, the adopt's occupancy check and `UpsertCampaign`'s ownership check
(`slotOwnershipQuery`), which is now the only thing enforcing one live row per slot on the
upsert's INSERT arm (`TestLiveStaleSlot2UpsertIsALostClaimWithoutTheLegacyIndex`).

**Tests restored** (from the pre-split contract commit, adapted): `TestLiveNewVersionCreatesSlotVersion2`
(slot 2 beside slot 1), `TestLiveConcurrentNewVersionClaims` (same-version race has one winner;
slots 3–8 at once each land exactly once); `TestMigrateRefusesADroppedDispatchIndex` now runs on a
scratch database against the slot-version index and replaces
`TestMigrateToleratesTheDroppedLegacySlotIndex`. The `new_version` API description and
api-catalog no longer mention the expand-phase refusal. Google stays outside
`model.ProviderSupportsSlotVersions`.

**Review fix.** Without its `requiredIndexes` entry, an INVALID copy of the legacy index (what a
failed `000050` down leaves) fell through to `migrationIndexOwners` and was advised "force 21",
replaying 000022–000050. New `migrationIndexDrops` makes `indexRecovery` answer "migration 000050
drops this; drop it, leave the schema version alone" for any index dropped after its last create
(`TestIndexRecovery_ADroppedIndexIsNotForcedBack`). See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md),
[internal/service](../code/internal-service.md) and [deployment](../kubernetes/deployment.md).
