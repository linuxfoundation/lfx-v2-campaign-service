-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- creative_assets: an uploaded image, subordinate to a brief, that a Meta ad creative
-- references by id. It stores the SOURCE BYTES, not a platform handle: Meta's image_hash
-- is per-ad-account and only knowable at dispatch, so the bytes must survive the gap
-- between upload and campaign create. Single-datastore model -- the bytes live in Postgres
-- (BYTEA), not external object storage.
--
-- hierarchy: Project -> Brief -> CreativeAssets (a brief may have several images over time).
--
-- GROWTH AND RETENTION -- recorded here because this table has no prune and the two other
-- growing tables in this schema do (PruneTerminalJobs behind 000026's index, and the outbox
-- sweep). Nothing removes a creative asset today, and NOTHING BOUNDS THIS TABLE. The UNIQUE
-- (brief_id, checksum) dedupe is not a bound: it collapses a re-upload of the SAME image to one
-- row, but a brief can still accumulate unlimited DISTINCT images, and each row is a raw blob
-- rather than the small metadata row the other growing tables add. Briefs are never
-- hard-deleted (ArchiveBrief is a soft status flip), so there is no orphan path and no ON
-- DELETE clause to choose -- which also means an archived brief's images are retained forever.
--
-- Two things are therefore owed and are deliberately NOT in this migration, because both need
-- decisions this service has not made: a per-brief CAP (enforced by the upload endpoint, which
-- lands in the follow-on PR alongside the request-size limit) and a PRUNE (assets under briefs
-- archived beyond a retention window, which needs a retention policy that does not exist yet).
-- They are written down here so the next reader finds the exposure already named rather than
-- discovering it from a disk alert.
--
-- Insert-only: an asset's bytes are immutable once stored, so there is no version or
-- updated_at/updated_by column (unlike campaign_briefs / campaigns / campaign_audiences).
-- A re-upload is resolved by the UNIQUE (brief_id, checksum) key -- the endpoint returns
-- the existing row -- rather than by mutating a stored asset.
CREATE TABLE IF NOT EXISTS creative_assets (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- project_id is a slug/UUID string (TEXT), matching campaign_briefs.project_id
    -- (converted to TEXT in migration 000003) and campaign_audiences. A UUID column would
    -- reject slug-scoped uploads like 'cncf' with an invalid-UUID error.
    project_id  TEXT        NOT NULL,
    brief_id    UUID        NOT NULL,
    -- mime_type is the VERIFIED type -- sniffed from the bytes by the upload handler, not
    -- the client's declared header -- constrained to the image formats a single-image ad
    -- creative supports. model.MimeTypePNG/MimeTypeJPEG mirror this set in Go, and the
    -- upload contract's Enum will mirror it a third time when that endpoint lands; the
    -- three are the same allow-list enforced at decode, at handler validation, and here.
    mime_type   TEXT        NOT NULL CHECK (mime_type IN ('image/png','image/jpeg')),
    -- byte_size is len(bytes), stored so callers and metrics can read the size WITHOUT
    -- loading the blob. That is exactly why a wrong value is dangerous here: no reader that
    -- uses the column as intended would ever notice it disagreed with the image. CHECK
    -- CHECK (byte_size = octet_length(bytes)) makes the column's whole PURPOSE an invariant of
    -- the table rather than a promise the writer makes. A separate CHECK (byte_size >= 0) was
    -- briefly here too and has been REMOVED as redundant: octet_length() is never negative, so
    -- the equality already implies non-negativity, and deleting the >= 0 clause broke no test --
    -- which is the definition of a constraint that does nothing. It matters because
    -- CreateAsset binds a.ByteSize and a.Bytes as INDEPENDENT parameters -- nothing in the
    -- insert derives one from the other -- so without it a buggy caller or any direct writer
    -- can persist a size that does not describe the blob, and no reader would notice: reading
    -- this column INSTEAD of the blob is exactly what it is for.
    --
    -- It is close to free. octet_length(bytea) is byteaoctetlen, which reads the size from the
    -- varlena header via toast_raw_datum_size and does NOT detoast: measured here over 150 MB
    -- of TOASTed rows it took 0.27 ms, against 179 ms for md5() on the same rows, which does
    -- detoast. An earlier revision of this comment claimed the CHECK would "detoast and hash
    -- the full image"; that was wrong, and the measurement is recorded so the claim is not
    -- re-derived from intuition.
    --
    -- An upper bound is deliberately NOT set here -- it has to equal the upload endpoint's request
    -- limit, and that endpoint does not exist yet, so it lands with it rather than being
    -- guessed now and silently disagreeing later.
    byte_size   BIGINT      NOT NULL CHECK (byte_size = octet_length(bytes)),
    -- checksum is the lowercase-hex SHA-256 of bytes. It is the dedupe key within a brief
    -- (the UNIQUE below) and the idempotency key the Meta client uses to avoid re-uploading
    -- the same image to an ad account.
    checksum    TEXT        NOT NULL,
    bytes       BYTEA       NOT NULL,
    created_by  JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The same image re-uploaded to a brief is ONE row: the upload endpoint returns the
    -- existing asset on conflict rather than storing a second copy of the bytes.
    UNIQUE (brief_id, checksum),
    -- COMPOSITE parent FK, not a brief_id-only one, for the reason migration 000007 gives
    -- when it upgraded campaign_audiences: this row copies project_id, and GetAsset TRUSTS
    -- that stored project_id for tenant scoping. A brief_id-only FK proves the brief exists
    -- but leaves the copied project_id unchecked, so a worker, backfill, or direct write
    -- could persist an asset whose project_id names a DIFFERENT project than its brief --
    -- and the asset would then read out under the wrong tenant. CreateAsset's INSERT ...
    -- WHERE EXISTS gate already refuses that on the API path, but the database is meant to
    -- be the source of truth for ALL writers, not just this one. The referenced
    -- UNIQUE (id, project_id) on campaign_briefs was added by 000007. Depending on that
    -- constraint means 000007's DOWN migration cannot run while this table exists (it would
    -- drop the index this FK needs). campaign_audiences already made the same constraint
    -- load-bearing, so this adds no new class of coupling -- and unwinding 21 versions is not
    -- an operation this service performs -- but the dependency is real, so it is recorded
    -- here rather than discovered during a rollback.
    FOREIGN KEY (brief_id, project_id) REFERENCES campaign_briefs (id, project_id)
);
-- No standalone brief_id index: UNIQUE (brief_id, checksum) already indexes brief_id as its
-- leftmost column. No project_id index either: an asset is always read by id (at dispatch)
-- or by (brief_id, checksum) (idempotent upload), never by project_id alone -- it is only a
-- tenant-scoping predicate on those reads -- so an index would carry write cost with no read
-- to serve. This differs from campaign_audiences, which is genuinely listed by project.
