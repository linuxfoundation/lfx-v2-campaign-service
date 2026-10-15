# 2026-10-15 — The lock release boots without the legacy slot index

**Fix** — review of #260. The lock release is staged as the image-only rollback target for the
later release that drops 000022's `uq_campaigns_brief_platform_variant_live`, but `Migrate`'s
required-index check still listed that index as required, so this image returned
`ErrMissingRequiredIndex` on the contracted schema and startup (`container.go`) closed the pool —
the rollback would boot-loop. `requiredIndex` gains an `optional` flag: an optional index's
ABSENCE is a healthy boot, a PRESENT one is still validated in full (invalid or wrong-definition is
still refused). The legacy index is the only optional entry; the four-column
`uq_campaigns_brief_platform_variant_slot_version_live` stays required. New
`RequiredIndexIsOptional` export lets the drop-each-index live tests branch off the real list.
`TestMigrateRefusesADroppedDispatchIndex` is replaced by
`TestMigrateToleratesTheDroppedLegacySlotIndex` (scratch database: impostor under the legacy name
→ `ErrRequiredIndexMismatch`; legacy dropped → `Migrate` succeeds; slot-version index dropped too
→ `ErrMissingRequiredIndex` naming only it), and `contractedSchemaRepo` now re-runs `Migrate` after
its drop instead of handing the schema straight to the repository. See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md) and
[deployment](../kubernetes/deployment.md).
