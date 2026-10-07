# 2026-10-07 — Point the old-index drop timeline at the lock release

**Docs** — review of #260. `domain.ErrSlotVersionUnavailable`'s comment said the legacy slot
index is dropped the release after migration `000037`. With the per-slot lock staged in its own
release, the drop now ships one release AFTER the lock (expand → lock → contract); the comment
says so. Migrations `000036`/`000037` carry the same superseded wording, but they are applied
(v1.0.17) and the migrations README forbids editing an applied migration, so the postgres concept
records that their timeline is superseded instead. See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md).
