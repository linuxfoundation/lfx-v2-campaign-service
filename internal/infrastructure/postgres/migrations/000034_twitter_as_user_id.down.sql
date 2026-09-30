-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the column discards every declared publishing identity, and they cannot be
-- recovered from anywhere else in this service. The effect is not a broken connection --
-- creates keep working -- which is what makes it worth stating: the connection silently
-- returns to accepting whichever handle the CALLER names, so the confused-deputy path this
-- column closes is reopened without anything failing to announce it.
ALTER TABLE twitter_ads_connections DROP COLUMN as_user_id;
