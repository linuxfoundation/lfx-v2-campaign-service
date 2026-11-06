-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop 000056's composite tenant key. 000002's id-only campaigns_brief_id_fkey was never dropped,
-- so the table keeps a brief reference throughout.
--
-- Always applies and loses nothing: the composite key only ever refused rows.
ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_brief_project_fkey;
