# 2026-10-15 — The newer-schema startup path trusts only the next release's version

**Fix** — review of #260 (LFXV2-2023).

- `schemaIsNewerThanImage` accepted any clean recorded version above the image's highest embedded
  one, although the expand/contract guarantee it rests on is one release apart. It now takes the
  skip-`Up()` path only for `recorded == latest+1`; a clean version further ahead falls through to
  `Up()` and its loud "no migration found for version N" refusal.
- `TestMigrateStartsOnACleanSchemaNewerThanThisImage` gains a latest+2 refusal subtest and an
  invalid-index subtest on the newer-schema path (a failed `CREATE UNIQUE INDEX CONCURRENTLY`
  → `ErrInvalidIndex`). Restoring the unbounded `recorded <= latest` check fails the latest+2 case.
- `LatestMigrationVersion` wraps an enumeration failure as "read the embedded migration versions",
  like `schemaIsNewerThanImage` does.
- The newer-schema-rollback log entry (this PR's own, unmerged) no longer cites a literal version.
- Updated [internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md) and
  [deployment](../kubernetes/deployment.md).
