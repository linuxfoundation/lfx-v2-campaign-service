# 2026-10-23 — order a creative-asset delete against an in-flight dispatch claim

**Fix** — `DeleteAsset` now locks the parent brief and runs its three-CTE delete as the SECOND
statement of that transaction; `000047`'s dimensions CHECK spells out its `IS NOT NULL`s; and
`createCreativeAssetQuery` decides both dimension columns with ONE predicate over BOTH parameters
instead of a `NULLIF` per column. Three defects in the creative-asset library, each one a thing
only a live database can decide, so each one also gained a `dbtest` test that fails without it.

**One snapshot is necessary and not sufficient.** The delete was a single statement by design —
the reference check and the delete in one snapshot, so no claim can appear between them. That is
true and it is not enough. Under READ COMMITTED a statement's snapshot is taken when the STATEMENT
begins, so the statement says nothing about a claim that commits an instant after that: the claim
is invisible to `referenced`, the delete proceeds, and the dispatcher later resolves a campaign
whose asset is gone — a failed dispatch instead of the clean 409 the caller should have had. The
fix is a brief-row `FOR UPDATE` taken first, which works because of what it CONFLICTS with rather
than what it excludes: `claimCampaignDispatchQuery`'s INSERT takes `FOR KEY SHARE` on that same
brief row as its foreign-key check and holds it to COMMIT, and `FOR UPDATE` blocks `FOR KEY SHARE`.
A claim that has already inserted therefore forces the delete to wait for it. The second half is
that the delete must be the NEXT statement: run as the locking statement itself it would keep the
snapshot the transaction already had and reintroduce exactly the race the lock was taken to close.
Lock ORDER is unchanged — `AdoptCampaign` fixes slot → brief for every transaction taking both, and
both asset writers take the brief lock and nothing else, so neither can close that cycle.

**A CHECK accepts a NULL result.** `CHECK ((width IS NULL AND height IS NULL) OR (width > 0 AND
height > 0))` reads as refusing a half-known pair and does the opposite. A CHECK admits anything
that is not false; for `(1200, NULL)` the second arm evaluates to `NULL`, so the constraint is
`false OR NULL` = `NULL` and Postgres ACCEPTS the row — the one shape the constraint exists to
refuse was the one shape that got through. The explicit `IS NOT NULL` conjuncts make that arm false
instead, while `(NULL, NULL)` still satisfies the first arm untouched.

**Per-column `NULLIF` decides each dimension alone.** Width and height are a PAIR, so
`NULLIF($6::int, 0), NULLIF($7::int, 0)` turns a caller's `(1200, 0)` into an attempted
`(1200, NULL)` — which the corrected CHECK refuses, so a legitimately dimensionless write fails
with SQLSTATE `23514` rather than storing an honest unknown. One `CASE WHEN $6::int > 0 AND
$7::int > 0` predicate over both parameters stores `(NULL, NULL)` instead.

Four tests under `internal/infrastructure/postgres/dbtest/`, because these properties need a
database AND a second connection — the in-package live suite cannot hold an uncommitted
transaction on one connection while the call under test runs on another:

- `TestLiveDeleteAssetWaitsForAnInFlightDispatchClaim` — holds an uncommitted claim insert, proves
  `DeleteAsset` is BLOCKED on that backend via `waitForBackendsBlockedBy` (positive evidence from
  `pg_blocking_pids`, not a timeout), then commits the claim and requires `ErrCreativeAssetInUse`
  with the asset still present.
- `TestLiveDeleteAssetSucceedsOnceTheClaimIsDeleted` — the contrast that keeps the above honest; a
  `DeleteAsset` that simply always refused could not pass the pair.
- `TestLiveDimensionsCheckRefusesAHalfKnownPair` — direct inserts, requiring SQLSTATE `23514` on
  `creative_assets_dimensions_positive` for `(1200, NULL)`, `(NULL, 628)`, `(1200, 0)` and
  `(1200, -628)`, and acceptance for `(NULL, NULL)` and `(1200, 628)`.
- `TestLiveCreateAssetConvertsAHalfKnownPairToUnknown` — reads the RAW columns, because
  `creativeAssetCols` wraps both in `COALESCE(..., 0)` and a stored `0` is indistinguishable from a
  stored `NULL` through the repository.

Each was proven able to fail by mutating the code it covers — removing the lock, moving the delete
back onto the locking statement, dropping the `IS NOT NULL`s, and restoring the per-column
`NULLIF`s — and observing that test fail at its own assertion with its own message. The mutations
were reverted.

The existing `TestDimensionsCheckRejectsAHalfKnownPair` deferral now names its covering live test
rather than deferring to nothing, which is what makes its own claim checkable.
