<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Database migrations

Hand-written SQL migrations applied by [golang-migrate](https://github.com/golang-migrate/migrate),
embedded via `migrations.go` and run through `postgres.Migrate`. One ascending,
linear stream — the version number **is** the ordering.

They run in an ArgoCD **PreSync Job** — `campaign-service migrate`, a subcommand of the
serving image (`charts/lfx-v2-campaign-service/templates/migrate-job.yaml`) — **before** each
rollout. The server itself no longer migrates at boot; it only **verifies** the schema
(`postgres.VerifySchema`) and never mutates it.

## The rule that matters most: expand/contract, one release apart

> A migration that **removes or narrows** something the N-1 release's SQL depends
> on ships **one release after** the code change that stopped depending on it.

- **Release N:** add the new shape (index, column, constraint) and move every read
  and write onto it. Keep the old shape.
- **Release N+1:** drop or narrow the old shape, once no running binary reads it.

Why: migrations run in a PreSync Job (linuxfoundation/lfx-self-serve#1543) **before** the
rollout, while the previous release's pods are still serving. A rolling deploy can therefore
briefly run the **N-1 binary against the N schema**. If release N's migration dropped what
the N-1 code depends on, those still-serving pods break for the length of the deploy.
Shipping the removal a release later guarantees no live binary depends on the old shape when
it disappears.

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

When a change genuinely cannot be staged apart, the rollout timing does not save it.
Migrations run in a PreSync Job that applies them **before** the Deployment sync, while the
N-1 ReplicaSet is still serving — so `strategy.type: Recreate` (which only orders the
Deployment's own pod swap, during Sync) does **not** cover an unstageable migration. The
remedies are then explicit old-pod shutdown (scale the old Deployment to zero) or a
maintenance window. That is the exception; the pattern is expand/contract, which is now the
whole safety mechanism. (Historically the chart pinned `Recreate` because migrations ran at
boot, where terminating the old pod first did keep it off the new schema; that pin is
transitional and removed in linuxfoundation/lfx-self-serve#1544.)

## Authoring checklist

- [ ] New numbered `NNNNNN_name.up.sql` + `.down.sql` pair. **Never edit an applied
      (merged) migration** — applied versions are never re-run; a change is always a
      new version.
- [ ] If this migration **removes or narrows** existing schema: confirm the code that
      stopped depending on it already shipped in an earlier release. If it genuinely cannot
      be staged and must ship here, it is NOT covered by the PreSync Job alone (which
      migrates while the old pods still serve) — it needs an explicit old-pod shutdown
      (scale to zero) or a maintenance window.
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
