-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping these DESTROYS the audit trail: the actor is recorded nowhere else,
-- because under system accounts the ad platform cannot supply it. For campaigns that
-- means losing the only record of who authorized paid spend. Down is provided for
-- migration symmetry, not as a routine operation.

ALTER TABLE campaigns
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS updated_by;
