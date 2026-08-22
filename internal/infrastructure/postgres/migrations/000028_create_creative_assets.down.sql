-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the table DISCARDS every uploaded image together with its checksum/dedupe
-- state. The bytes exist nowhere else in this service -- an operator uploaded them, and
-- the ad-platform image_hash resolved at dispatch is a derived per-account handle, not the
-- source image. A re-migration leaves every brief that referenced an asset unable to build
-- its image creative until the images are uploaded again.
DROP TABLE IF EXISTS creative_assets;
