# 2026-08-11 — LFXV2-3056: register the adoption index in requiredIndexes

**Fix** — a merge-order defect: neither branch was wrong on its own, and the resulting tree
booted with an unregistered uniqueness guard.

Migration `000020` creates `uq_campaigns_platform_campaign_live` on this branch. `main`
separately gained the boot-time `requiredIndexes` registry in
`internal/infrastructure/postgres/pool.go`, whose membership rule is "every unique PARTIAL
index, because no constraint backs one and a `DROP INDEX` removes it silently". The two
changes touch different files, so the merge was textually clean and the registry simply did
not know about the index this branch adds.

`TestEveryUniquePartialIndexIsRequired` (dbtest) caught it, because it enumerates the unique
partial indexes from `pg_index` in the MIGRATED schema rather than from a hand-written list.
A registry test driven off the registry would have passed.

## The predicate was measured, not derived

`checkRequiredIndexes` compares against `pg_get_expr(indpred, indrelid)` — the DEPARSED
predicate, not the migration's source text. For

    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'google-ads'

PostgreSQL 16 renders

    ((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND (platform = 'google-ads'::text))

— each conjunct parenthesised and the whole wrapped once. That was obtained by building the
index against a live PostgreSQL 16 and reading `pg_get_expr` back, not by hand-deriving the
spelling. A guessed predicate does not fail the way a guessed name does: the index is present
and correct, the check reports `predicate ... want ...`, and the service refuses to boot on a
schema that is actually fine.

## Why it did not fail locally before CI

`go test ./...` with `TEST_DATABASE_URL` unset SKIPS both live-Postgres packages and still
prints `ok`. The failing test is one of them. Verified here against a local PostgreSQL 16 with
`-p 1`, both directions: the test FAILS with the registry entry removed, naming the index, and
passes with it.
