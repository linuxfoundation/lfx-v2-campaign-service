# 2026-10-22 — persist a creative asset's pixel dimensions at upload

**Update** — `creative_assets` now records `width`/`height`, written on the same INSERT that
stores the bytes. Migration `000047` adds both columns NULLABLE with no default, plus
`CHECK ((width IS NULL AND height IS NULL) OR (width > 0 AND height > 0))`;
`model.CreativeAsset` carries the pair as `int`; `CreativeAssetRepo` binds `NULLIF($n::int, 0)`
on write and reads `COALESCE(width, 0)` back; and `design/brief.go`'s `CreativeAsset` result
type publishes them as OPTIONAL `Minimum(1)` attributes.

The handler was already computing these numbers and throwing them away. `image.DecodeConfig` is
stage 1 of `UploadCreativeAsset`'s validation, and `cfg.Width`/`cfg.Height` feed both the
decompression-bomb gate and the decode-budget reservation before being discarded along with the
decoded image. The per-platform slot classifier — which slot an image can fill is a question
about its shape, nothing else — would otherwise have to detoast a blob of up to 30 MiB and
re-decode a header this request already parsed, per asset, per plan.

"Unknown" needed ONE meaning across four layers, and it has one: DB `NULL` ↔ model `0` ↔ wire
ABSENT. Each hop has a failure mode if it drifts.

- **NULL, not a default.** Nullable is what makes `000047` an EXPAND step under the
  expand/contract rule: an N-1 binary's insert names neither column. `NOT NULL` would need a
  default, and the only honest default for "how wide is this image" is "unknown".
- **Not zero.** A reader that coerced `NULL` to `0` reports every pre-`000047` asset as failing
  every slot's minimum — strictly worse than reporting it as unclassified, because it looks like
  a measurement.
- **`NULLIF` on write.** A bare `$n` would persist a literal `0` for a dimensionless model,
  which the CHECK refuses outright, turning a legitimate write into an insert failure.
- **Absent on the wire.** `creativeAssetResult` publishes the pair only when both are positive,
  and copies to locals rather than pointing into the caller's model.

The CHECK is a narrowing and owes `000029`'s N-1 argument; it is trivial here, since an N-1
INSERT lands `(NULL, NULL)` and every pre-migration row already is `(NULL, NULL)`. Nothing can
violate it, so it is a plain CHECK rather than `NOT VALID` + `VALIDATE`.

Two things were deliberately NOT done. **No backfill**: the values must come from the same
`image/png` and `image/jpeg` decoders the upload path uses, so a backfilled row and a fresh one
classify identically — and Postgres has no image decoder, so there is nothing a SQL-side
migration could run. It is an application step for a later release, as is `NOT NULL` if it is
ever wanted. **No backfill on re-upload either**: the `ON CONFLICT` arm stays the deliberate
no-op it was, because filling dimensions there would contradict the table's stated insert-only
immutability and would cover only the arbitrary subset of assets somebody happened to re-send.

**No index.** My own design note had specified one on `(brief_id)`; reading `000028` showed
`UNIQUE (brief_id, checksum)` already indexes `brief_id` as its leftmost column, so it would
have been a duplicate. Neither dimension is ever a search predicate — a slot rule is a ratio
with a tolerance plus two minimums, decided in Go over assets already loaded.

Tests: `TestMigration000047_IsExpandOnly` (no `NOT NULL`/`DEFAULT`, only the two columns, only
`creative_assets`, no index, and a down that drops neither more nor the table);
`TestCreativeAssetColumnsCarryDimensions` (the `NULLIF`, and the positional column order the
scanners bind); `TestBriefService_UploadCreativeAsset_PersistsHeaderDimensions`, which uses a
3x7 fixture on purpose — every other fixture in that file is 2x2 and cannot detect a
width/height transposition; and `TestCreativeAssetResult_UnknownDimensionsAreAbsentNotZero`
over all four known/unknown combinations.
