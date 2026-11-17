-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore 000028's image-only allow-list exactly as version 56 has it: a plain, validated CHECK,
-- so this file is the exact inverse of the up migration. Because the CHECK is validated, the
-- rollback refuses while any video/mp4 row is stored. That is deliberate and the safer behaviour:
-- a rollback never deletes an asset or leaves rows the restored schema does not admit. Remove or
-- move the video rows first, then roll back.
ALTER TABLE creative_assets
    DROP CONSTRAINT IF EXISTS creative_assets_mime_type_check;
ALTER TABLE creative_assets
    ADD CONSTRAINT creative_assets_mime_type_check
    CHECK (mime_type IN ('image/png','image/jpeg'));
