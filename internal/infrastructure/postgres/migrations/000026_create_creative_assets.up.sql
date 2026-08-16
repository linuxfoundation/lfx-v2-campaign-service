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
    brief_id    UUID        NOT NULL REFERENCES campaign_briefs(id),
    -- mime_type is the VERIFIED type -- sniffed from the bytes by the upload handler, not
    -- the client's declared header -- constrained to the formats a Meta single-image
    -- creative supports. The same set is the Enum on the upload contract (design/brief.go).
    mime_type   TEXT        NOT NULL CHECK (mime_type IN ('image/png','image/jpeg')),
    byte_size   BIGINT      NOT NULL,
    -- checksum is the lowercase-hex SHA-256 of bytes. It is the dedupe key within a brief
    -- (the UNIQUE below) and the idempotency key the Meta client uses to avoid re-uploading
    -- the same image to an ad account.
    checksum    TEXT        NOT NULL,
    bytes       BYTEA       NOT NULL,
    created_by  JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The same image re-uploaded to a brief is ONE row: the upload endpoint returns the
    -- existing asset on conflict rather than storing a second copy of the bytes.
    UNIQUE (brief_id, checksum)
);
-- No standalone brief_id index: UNIQUE (brief_id, checksum) already indexes brief_id as its
-- leftmost column. No project_id index either: an asset is always read by id (at dispatch)
-- or by (brief_id, checksum) (idempotent upload), never by project_id alone -- it is only a
-- tenant-scoping predicate on those reads -- so an index would carry write cost with no read
-- to serve. This differs from campaign_audiences, which is genuinely listed by project.
