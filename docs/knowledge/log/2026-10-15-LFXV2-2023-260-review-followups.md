# 2026-10-15 — The newer-schema tolerance is one hop, in every description

**Docs** — two review threads on #260 (LFXV2-2023) that arrived as it merged.

- `LatestMigrationVersion`'s godoc called the embedded head "the boundary above which `Migrate`
  treats a clean recorded version as a newer release's schema" — the unbounded rule
  `schemaIsNewerThanImage` no longer follows. It now says `latest+1` is the only newer schema
  `Migrate` trusts without running `Up()`, and that a clean version further ahead is refused through
  `Up()`.
- [The rollback entry](2026-10-15-LFXV2-2023-newer-schema-rollback.md) still says the tolerance
  "is not bounded to known-compatible versions". That sentence is superseded: since
  [the one-hop entry](2026-10-15-LFXV2-2023-newer-schema-one-hop.md) the skip-`Up()` path takes a
  CLEAN recorded version exactly one past the embedded head, and a DIRTY one, or one that is two or
  more versions ahead, goes through `Up()` and is refused.
- No code behaviour changed.
