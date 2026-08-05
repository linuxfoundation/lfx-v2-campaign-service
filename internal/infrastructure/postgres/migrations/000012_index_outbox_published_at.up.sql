-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Revisits the "no index on published_at" call in 000010: that was verified against a table
-- where published rows dominate, so the PK scan finds a full LIMIT batch almost immediately.
-- On a large, mostly-PENDING outbox the same query has to walk id-order past a long run of
-- unpublished rows before it finds enough matches, which is exactly the shape the retention
-- prune sees every 15s during a backlog. A partial index over only PUBLISHED rows gives that
-- pass a direct path to (or a cheap proof of the absence of) prunable rows without paying for
-- the pending rows in between.
CREATE INDEX IF NOT EXISTS idx_index_outbox_published
    ON index_outbox (published_at)
    WHERE published_at IS NOT NULL;
