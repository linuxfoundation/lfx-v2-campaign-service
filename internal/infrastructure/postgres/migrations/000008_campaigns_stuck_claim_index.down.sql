-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- CONCURRENTLY for the same reason as the up migration: a plain DROP INDEX takes a
-- lock that blocks writes on campaigns, and a rollback runs while other replicas are
-- still dispatching. Also drops the INVALID index a failed concurrent build leaves
-- behind, so a retry of the up migration is clean.
DROP INDEX CONCURRENTLY IF EXISTS idx_campaigns_stuck_claims;
