-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Admit video/mp4 creative assets (#586). A LinkedIn video Sponsored Content variant names a
-- stored MP4 by linkedInConfig.variants[].videoAssetId, and at dispatch its bytes are uploaded
-- through LinkedIn's Videos API. The bytes still live here (BYTEA), so the existing per-row
-- ceiling (000029, 30 MiB) and the per-brief quota bound a video exactly as they bound an image.
--
-- 000028 declared the allow-list as an UNNAMED column CHECK, which Postgres names
-- creative_assets_mime_type_check. It is replaced, not widened in place, because a CHECK cannot
-- be altered. A video carries no pixel dimensions from the upload handler, so width/height stay
-- NULL, which 000047's pair CHECK already allows.
ALTER TABLE creative_assets
    DROP CONSTRAINT IF EXISTS creative_assets_mime_type_check;
ALTER TABLE creative_assets
    ADD CONSTRAINT creative_assets_mime_type_check
    CHECK (mime_type IN ('image/png','image/jpeg','video/mp4'));
