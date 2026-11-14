-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore 000028's image-only allow-list. NOT VALID so the rollback never fails on (or deletes)
-- a video/mp4 row stored while 000057 was applied: existing rows are kept unchecked, and only
-- new writes are held to the image-only list again.
ALTER TABLE creative_assets
    DROP CONSTRAINT IF EXISTS creative_assets_mime_type_check;
ALTER TABLE creative_assets
    ADD CONSTRAINT creative_assets_mime_type_check
    CHECK (mime_type IN ('image/png','image/jpeg')) NOT VALID;
