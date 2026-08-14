# 2026-08-13 — LFXV2-3257 campaign variant slot

**Fix** — A brief could hold only ONE campaign per platform, so a Demand Gen
dispatch on a brief that already had a Search campaign reported success and
returned the SEARCH campaign's platform id without ever calling Google. Observed
end to end: the job succeeded, the database stayed at one row.

Both are `platform = 'google-ads'`, and `campaigns` was unique on `(brief_id,
platform)` — so `dispatchPlatform`'s idempotency fast path, which exists to stop
a retry creating a duplicate paid campaign, could not tell the two channels
apart and treated the second as a retry of the first.

`campaigns` gains `variant`: which of a platform's campaign types this row is.
The slot key becomes `(brief_id, platform, variant)`. Google's UI offers Search
and Demand Gen as simultaneous checkboxes and Performance Max is coming, so one
brief legitimately holds several google-ads campaigns; every other provider uses
`'default'` and is unchanged in behaviour. Meta's and Reddit's `objective`
configures a single campaign rather than multiplying it.

Named `variant`, not `channel`: `channel` is Google's word and `objective` is
Meta's and Reddit's. The column names the concept, so each new Google channel is
a value rather than a migration.

**Note** — The key had to widen in three places at once or it breaks. The unique
index is not only a uniqueness rule: `ClaimCampaignDispatch` infers it as the
arbiter of `INSERT ... ON CONFLICT ... DO NOTHING`, which is what elects a
single dispatch winner across replicas. A conflict target that names different
columns than the index matches no arbiter and fails at runtime with "there is no
unique or exclusion constraint matching the ON CONFLICT specification". So the
index (000022), the claim/upsert/adopt conflict targets, and the idempotency
lookup in `dispatchPlatform` all moved together.

Four migrations rather than one, because `DROP INDEX CONCURRENTLY` cannot run
inside a transaction and a `DO $$` guard is one: 000021 adds the column, 000022
builds the widened index CONCURRENTLY, 000023 VERIFIES that index by definition
— present, valid, unique, correctly keyed, partial — and raises otherwise, and
000024 does the bare drop that is reached only if the guard passed. This mirrors
000013/000014, which set out the same constraints.

An absent `channel` maps to `'default'`, NOT to `'search'`. Every caller written
before this column omits it and its rows were backfilled to `'default'`, so
mapping absence to `'search'` would put a retry in a different slot from the
campaign it is retrying — and create the duplicate the slot key exists to
prevent.
