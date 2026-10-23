-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Persists the pixel dimensions the upload handler ALREADY computes and then discards.
--
-- internal/service/creative_asset.go calls image.DecodeConfig on the uploaded bytes before it
-- decodes them, uses the resulting cfg.Width/cfg.Height for the decompression-bomb gate
-- (dimensionsWithinLimits) and the decode-budget reservation (decodedBytesFor), and then throws
-- both numbers away. Every later reader that needs an image's shape -- the per-platform slot
-- classifier, the asset plan, the UI's gap report -- would otherwise have to re-read the blob out
-- of BYTEA and re-decode its header just to recover a pair of integers the service held in a local
-- variable at insert time. These two columns are that pair, written on the same INSERT.
--
-- WHY NULLABLE, AND WHY THAT IS NOT A DEFERRED "NOT NULL".
--
-- This is an EXPAND step under the expand/contract rule in README.md: adding a nullable column
-- widens the table, which an N-1 binary tolerates, because the previous release's
-- createCreativeAssetQuery names its columns explicitly and simply does not mention these two.
-- Declaring them NOT NULL here would make that same N-1 INSERT fail -- it supplies no value and
-- there is no DEFAULT that could be correct, since the only honest default for "how wide is this
-- image" is "unknown". NULL IS that answer, and it is the one rows written before this release
-- legitimately carry.
--
-- So NULL has a MEANING here and is not merely a migration artifact: it is "this row was stored
-- before the service recorded dimensions". Readers must treat it as unknown rather than as zero --
-- a 0x0 image is not a thing, and a classifier that coerced NULL to 0 would silently report every
-- pre-existing asset as failing every slot's minimum, which is worse than reporting it as
-- unclassified.
--
-- A BACKFILL IS A SEPARATE STEP, deliberately. Filling these columns means detoasting and
-- header-decoding every stored blob, which is real I/O against a table whose rows are up to 30 MiB
-- (000029), and it is an application concern rather than a SQL one: the width and height must come
-- from the SAME image/png and image/jpeg decoders the upload path uses, so that a backfilled row
-- and a freshly uploaded one are classified identically. A SQL-side backfill cannot do that at all
-- -- Postgres has no image decoder -- so there is nothing this migration could run even if it
-- wanted to. NOT NULL, if it is ever wanted, belongs in a LATER release, after a backfill has run
-- and after a release boundary has passed, per the same expand/contract rule.
--
-- NO INDEX IS ADDED. 000028 already settled the indexing argument for this table and its reasoning
-- still holds: UNIQUE (brief_id, checksum) indexes brief_id as its leftmost column, so a
-- list-by-brief read -- which is what the asset plan and the library view both are -- is already
-- served. Neither width nor height is ever a search predicate: the classifier reads the pair for
-- EVERY asset of a brief it has already loaded and decides in Go, because a slot's rule is a ratio
-- with a tolerance plus two minimums, which no btree on either column alone can answer.
ALTER TABLE creative_assets
    ADD COLUMN IF NOT EXISTS width  INTEGER,
    ADD COLUMN IF NOT EXISTS height INTEGER;

-- Dimensions are a property of the stored image, so -- exactly like byte_size's
-- CHECK (byte_size = octet_length(bytes)) -- the table, not only the writer, enforces that what is
-- recorded could describe a real one. What it refuses is a row that claims a dimension of zero or
-- a negative one, and a row that records one dimension without the other -- a half-known shape is
-- not classifiable and nothing should be able to persist it.
--
-- THE SECOND ARM SPELLS OUT ITS NOT NULLs, and they are load-bearing rather than redundant with
-- `> 0`. A CHECK admits anything that is not false, and in SQL an unknown operand poisons the
-- whole expression: for (1200, NULL) the first arm is plainly false, while the second evaluates
-- `1200 > 0 AND NULL > 0` to NULL, so the constraint's result is `false OR NULL` = NULL and
-- Postgres ACCEPTS the row. Writing it as `width > 0 AND height > 0` alone would therefore make
-- the half-known pair this constraint exists to refuse the one case that slips through. With the
-- NOT NULLs in front, that same row evaluates the second arm to false and is rejected, while
-- (NULL, NULL) still satisfies the first arm untouched -- which is what keeps the column nullable
-- for the pre-release rows it is nullable FOR.
--
-- A CHECK is a narrowing, so it owes the same N-1 argument 000029 made, and it is simpler here:
-- an N-1 INSERT names neither column, so both land NULL and satisfy the first arm. No existing row
-- can violate it either -- the columns are added by this same migration, so every existing row is
-- (NULL, NULL). It is stated as a plain CHECK rather than NOT VALID + VALIDATE CONSTRAINT, but be
-- precise about what that does and does not buy, because an earlier version of this comment was
-- wrong on both halves:
--
--   - Postgres DOES scan on ADD CONSTRAINT. The validation pass runs because the constraint is
--     added without NOT VALID, not because any row could fail it; "there is nothing to scan" was
--     false. What is true is that the scan is over a table whose rows are all (NULL, NULL), so it
--     finds nothing and the ACCESS EXCLUSIVE lock is held only for as long as that pass takes.
--   - NOT VALID does NOT leave a declared-but-unenforced window. It enforces the constraint on new
--     writes IMMEDIATELY and defers only the check of pre-existing rows. The reason to prefer a
--     plain CHECK here is the scan being empty, not any gap in enforcement.
--
-- Splitting this into ADD ... NOT VALID plus a VALIDATE CONSTRAINT would also not shorten the lock
-- as written, which is why it is not done as a follow-up edit to this file. golang-migrate ships a
-- migration file to a single ExecContext unless x-multi-statement is set -- Run() in
-- database/pgx/v5/pgx.go splits only when p.config.MultiStatementEnabled, and this repo's pool.go
-- never sets it -- so both statements would land in ONE implicit transaction and the ACCESS
-- EXCLUSIVE lock taken by the ADD would be held until that transaction committed, VALIDATE
-- included. Getting the weaker ShareUpdateExclusive lock for the validate pass requires the two
-- statements in SEPARATE migrations, which is a change to make when a table is big enough to
-- warrant it, not here.
-- DROP ... IF EXISTS before ADD, as 000041 does: ADD CONSTRAINT has no IF NOT EXISTS form, so
-- this is how a re-run of a partially-applied migration stays idempotent rather than failing on
-- a duplicate-object error.
ALTER TABLE creative_assets
    DROP CONSTRAINT IF EXISTS creative_assets_dimensions_positive,
    ADD CONSTRAINT creative_assets_dimensions_positive
    CHECK (
        (width IS NULL AND height IS NULL)
        OR (width IS NOT NULL AND height IS NOT NULL AND width > 0 AND height > 0)
    );
