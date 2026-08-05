-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore the pre-lease claim index before dropping the columns it does not reference, so a
-- rolled-back schema still serves the claim query rather than sequential-scanning the table.
DROP INDEX IF EXISTS idx_index_outbox_claimable;

CREATE INDEX IF NOT EXISTS idx_index_outbox_pending
    ON index_outbox (object_type, object_id, id)
    WHERE published_at IS NULL;

ALTER TABLE index_outbox
    DROP COLUMN IF EXISTS leased_until,
    DROP COLUMN IF EXISTS leased_by;
