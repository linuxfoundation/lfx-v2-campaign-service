-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Add campaign_audiences.include_list_ids: the EXISTING HubSpot contact lists an attached
-- audience sends to directly, when an operator nominates several of them instead of one master.
--
-- Why a second column rather than reusing platform_master_list_id. A HubSpot marketing email's
-- recipients (`to.contactIlsLists.include`) are an ARRAY, so a send can target several lists
-- without first composing a master list that unions them. Attaching existing lists that way
-- creates nothing in the portal; a single text pointer cannot record it.
--
-- platform_master_list_id is still written, and still required for a built row: it holds the
-- FIRST include id, so the CHECK constraint from 000006 (built => master id present) keeps its
-- meaning and every reader that knows only the master column still names a real recipient list.
-- Readers that know this column send to all of it (CampaignAudience.SendListIDs).
--
-- JSONB string array, the same encoding as suppression_list_ids. NULL means "no direct include
-- set": the audience sends to platform_master_list_id alone, which is what every row written
-- before this column existed does. Nothing is backfilled.
--
-- EXPAND-only and nullable with no default: the N-1 binary does not know the column, never
-- writes it, and its UPDATE leaves it untouched, so a rolling deploy is safe in both directions.
-- An N-1 dispatch reading a multi-include row sends to the first list only -- a subset, never a
-- wrong audience.

ALTER TABLE campaign_audiences
    ADD COLUMN IF NOT EXISTS include_list_ids JSONB NULL;

-- The shape is enforced in Go (SendListIDs fails closed on anything that is not a string array);
-- the database refuses a non-array too, so an out-of-band write cannot store one. The column is
-- new and NULL on every row, so validating the constraint scans nothing it can fail on.
ALTER TABLE campaign_audiences
    DROP CONSTRAINT IF EXISTS campaign_audiences_include_list_ids_is_array;
ALTER TABLE campaign_audiences
    ADD CONSTRAINT campaign_audiences_include_list_ids_is_array
    CHECK (include_list_ids IS NULL OR jsonb_typeof(include_list_ids) = 'array');
