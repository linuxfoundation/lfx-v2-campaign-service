-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the column discards the provenance it holds, and there is no other copy:
-- the credential that served a campaign is known only at dispatch time, so a re-apply
-- of 000027 comes back with every existing row NULL (unknown) rather than restored.
-- Rolling this back is therefore lossy for history already recorded, not just for the
-- schema — see the "Rolling back is not the deploy run backwards" note in the
-- deployment concept.
ALTER TABLE campaigns
    DROP COLUMN IF EXISTS ran_on_system_account;
