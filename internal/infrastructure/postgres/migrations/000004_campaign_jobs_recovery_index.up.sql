-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Support the stuck-job recovery query (JobRepo.FailStuckJobs), which runs on
-- startup AND every recoverySweepInterval (5m) on EVERY replica:
--
--   UPDATE campaign_jobs SET ... WHERE status IN ('queued','running')
--                                  AND updated_at < now() - <cutoff>;
--
-- 000002 only indexes brief_id, so this predicate does a full table scan of
-- campaign_jobs on every sweep. As terminal job history (succeeded/partial/
-- failed) accumulates that scan grows unbounded, even though the live set the
-- sweep actually touches (queued/running) stays tiny.
--
-- A PARTIAL index on updated_at, restricted to exactly the two non-terminal
-- statuses the predicate matches, keeps the index small (it never grows with
-- terminal history) and lets the sweep read only the candidate rows ordered by
-- updated_at. It is transparent to behavior — purely a performance fix.
--
-- Separate migration (not an edit to 000002): golang-migrate records applied
-- versions and never re-runs them, so amending 000002 would silently skip DBs
-- that already ran it.
CREATE INDEX IF NOT EXISTS idx_campaign_jobs_recovery
    ON campaign_jobs (updated_at)
    WHERE status IN ('queued', 'running');
