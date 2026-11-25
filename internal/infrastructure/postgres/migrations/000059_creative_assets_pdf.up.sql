-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Admit application/pdf creative assets (#665). A LinkedIn document ad names a stored PDF by
-- linkedInConfig.documentAssetId, and at dispatch its bytes are uploaded through LinkedIn's
-- Documents API. The bytes still live here (BYTEA), so the existing per-row ceiling (000029,
-- 30 MiB) and the per-brief quota bound a PDF exactly as they bound an image or a video.
--
-- The CHECK is replaced, not widened in place, because a CHECK cannot be altered (see 000057).
-- A PDF carries no pixel dimensions, so width/height stay NULL, which 000047's pair CHECK allows.
-- No IF EXISTS on the DROP, for 000057's reason: a name mismatch must fail loudly.
ALTER TABLE creative_assets
    DROP CONSTRAINT creative_assets_mime_type_check;
ALTER TABLE creative_assets
    ADD CONSTRAINT creative_assets_mime_type_check
    CHECK (mime_type IN ('image/png','image/jpeg','video/mp4','application/pdf'));
