# 2026-11-03 — #171 follow-up: the live prune test now catches a dropped batch LIMIT

**Fix** — test-only follow-up to the archived-brief asset prune (#451 review).

`TestLivePruneArchivedBriefAssetsDeletesOnlyPastTheWindow` asserted only that three expired
assets at a batch limit of two drained in at least two batches. That also passes when
`pruneArchivedAssetsQuery` loses its `LIMIT $2`: one unbounded delete of three, then an empty
batch, is two batches. `drainArchivedAssetPrune` now returns each batch's deleted count, and the
test asserts the first batch deleted exactly the limit and that no batch deleted more.

Mutation-proved against a local Postgres 16: with the query's `LIMIT $2` replaced by an
unbounded `LIMIT NULLIF($2::int, $2::int)`, the new assertion fails ("batch 1 deleted 8 rows,
over the limit of 2") while the old batch-count assertion passes. No production code changes.
