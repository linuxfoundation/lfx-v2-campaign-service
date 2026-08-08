-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping these DESTROYS the audit trail: the actor is recorded nowhere else,
-- because under system accounts the ad platform cannot supply it. Down is provided
-- for migration symmetry, not as a routine operation.

ALTER TABLE campaign_briefs
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS updated_by;
