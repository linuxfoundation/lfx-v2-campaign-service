-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Rebuild idx_campaigns_stuck_claims after 000009 drops an INVALID copy (or as a
-- no-op if the index is already valid, via IF NOT EXISTS). Kept in its own migration
-- file, mirroring 000008: the pgx/v5 golang-migrate driver executes each file with a
-- bare ExecContext and does NOT wrap it in a transaction, and CREATE INDEX
-- CONCURRENTLY cannot run inside one. Do NOT add other statements to this file — a
-- multi-statement migration would be batched and reintroduce the transaction
-- constraint (this is exactly the bug this file fixes: 000009 previously tried to
-- do the drop and the CONCURRENTLY rebuild in a single file).
--
-- This migration runs on the NEXT deploy after an operator `force`-clears a dirty
-- version 8 (golang-migrate's dirty-recovery marks the version applied WITHOUT
-- running it, so 000008's own IF NOT EXISTS is never re-evaluated) — so recovery
-- requires operator force + a subsequent deploy for this file to run and rebuild
-- the index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_campaigns_stuck_claims
    ON public.campaigns (created_at)
    WHERE status = 'pending';
