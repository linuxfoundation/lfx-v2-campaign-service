# 2026-10-15 — The lock release starts on a clean schema newer than its image

**Fix** — review of #260 (dealako and Copilot). Making the legacy slot index `optional` did not by
itself make the lock release an image-only rollback target: the release that drops the index also
advances `schema_migrations.version` past every migration this image embeds, and golang-migrate
v4.19.1's `Up()` returns "no migration found for version N" for a recorded version its source does
not contain — so `Migrate` failed before the index checks ran and startup closed the pool. The
earlier live test dropped the index without advancing the version, so it never reached that path.
`Migrate` now reads the recorded version first (`schemaIsNewerThanImage`): CLEAN and above the
highest embedded version (read from the migration source; exported as `LatestMigrationVersion`)
skips `Up()`, leaves the version untouched, logs a warning, and still runs `checkNoInvalidIndexes`.
A DIRTY newer schema and every other state go through `Up()` unchanged. The tolerance is not bounded
to known-compatible versions — the codebase has no such marker — and rests on expand/contract (a
release stays compatible with the next release's schema), documented in code and below. New live
test `TestMigrateStartsOnACleanSchemaNewerThanThisImage` (normal path; latest+1 clean with the legacy
index dropped → boot succeeds, version unchanged; dirty → `ErrDirty`; slot-version index dropped →
`ErrMissingRequiredIndex`); forcing the check off reproduces the reported "no migration found for
version 47". See [internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md)
and [deployment](../kubernetes/deployment.md).
