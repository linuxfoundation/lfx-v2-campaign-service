<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Database migrations

Hand-written SQL migrations applied by [golang-migrate](https://github.com/golang-migrate/migrate),
embedded via `migrations.go` and run through `postgres.Migrate`. One ascending,
linear stream — the version number **is** the ordering.

## The rule that matters most: expand/contract, one release apart

> A migration that **removes or narrows** something the N-1 release's SQL depends
> on ships **one release after** the code change that stopped depending on it.

- **Release N:** add the new shape (index, column, constraint) and move every read
  and write onto it. Keep the old shape.
- **Release N+1:** drop or narrow the old shape, once no running binary reads it.

Why: migrations run at pod boot today (until #1543 moves them into a PreSync
Job), and a rolling deploy can briefly run the **N-1 binary against the N
schema**. If release N already dropped what the N-1 code depends on, that old pod
breaks for the length of the rollout. Shipping the removal a release later
guarantees no live binary depends on the old shape when it disappears.

**"Stopped depending" means completely** — the N-1 binary must not rely on the old
shape for *any* row it can still touch, **including soft-deleted rows**.

### Case study: `000013` / `000014` (why this rule has teeth)

These two could **not** be split, and that is the cautionary tale. `000013` added
the partial unique index; `000014` dropped the old full `UNIQUE (brief_id, platform)`.
Deferring the drop to a later release — the usual expand/contract remedy — did not
work, because the old full constraint still governed **soft-deleted** rows the delete
path had to free: with the drop deferred, a re-dispatch after delete hit
`ON CONFLICT ... DO NOTHING` and was silently swallowed (`RowsAffected` 0). So the
delete endpoint would have shipped doing nothing.

When a change genuinely cannot be staged apart, a **rollout strategy** carries the
ordering instead — the chart pinned `strategy.type: Recreate` so the old pod is gone
before the new one migrates. That works **only while migrations run at boot**: `Recreate`
removes the old pod before the new one boots and migrates. Once migrations move into a
PreSync Job (linuxfoundation/lfx-self-serve#1543) — which runs **before** the Deployment
sync, while the N-1 ReplicaSet is still serving — `Recreate` no longer covers an
unstageable migration; that would need explicit old-pod shutdown (scale the old Deployment
to zero) or a maintenance window. That is the exception, not the pattern, and the reason
migrations are being moved out of boot: once no pod migrates the shared schema at boot,
expand/contract alone is sufficient and the strategy override goes away
(linuxfoundation/lfx-self-serve#1544).

## Authoring checklist

- [ ] New numbered `NNNNNN_name.up.sql` + `.down.sql` pair. **Never edit an applied
      (merged) migration** — applied versions are never re-run; a change is always a
      new version.
- [ ] If this migration **removes or narrows** existing schema: confirm the code that
      stopped depending on it already shipped in an earlier release, or that it ships
      here **and** a rollout ordering (e.g. `Recreate`) covers the overlap.
- [ ] `CREATE INDEX CONCURRENTLY` / `DROP INDEX CONCURRENTLY` lives **alone** in its
      file — a multi-statement migration is batched in an implicit transaction, which
      `CONCURRENTLY` forbids.
- [ ] A `.down.sql` exists and is correct; remember its safety is per-migration (see
      the "Rolling back is not the deploy run backwards" note in the deployment
      concept).
- [ ] If this migration adds a **unique index that stands in for a constraint** (one
      whose absence would be *silent* — every write it serializes still succeeds), register
      it in `requiredIndexes` so boot / `/readyz` fails closed when it is missing. That
      guard is unique-index-only; it does not model columns, `NOT NULL`, or foreign keys.

## References

- Concept: `docs/knowledge/code/internal-infrastructure-postgres.md`
- Rollout/rollback: `docs/knowledge/kubernetes/deployment.md`
