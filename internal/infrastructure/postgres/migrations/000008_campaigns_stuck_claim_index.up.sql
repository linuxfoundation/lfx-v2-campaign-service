-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Support the stuck-claim scan (CampaignRepo.StuckDispatchClaims), which runs on
-- startup AND every stuckClaimSweepInterval (5m) on EVERY replica:
--
--   SELECT ... FROM campaigns WHERE status = 'pending'
--                               AND created_at < now() - <cutoff>
--                             ORDER BY created_at ASC LIMIT $2;
--
-- Without this the predicate scans all of campaigns on every sweep. Campaigns are
-- append-mostly and terminal rows (created / created_degraded / failed) accumulate
-- forever, so that scan grows unbounded — while the set the sweep actually cares
-- about ('pending' claims) stays tiny and is usually EMPTY.
--
-- A PARTIAL index on created_at restricted to status = 'pending' keeps the index
-- small (it never grows with terminal history) and also serves the ORDER BY
-- created_at ASC directly, so the LIMIT can stop early instead of sorting the
-- whole match set. Transparent to behavior — purely a performance fix.
--
-- This mirrors idx_campaign_jobs_recovery (000004), which exists for the same
-- reason on the analogous stuck-JOB sweep.
--
-- Separate migration (not an edit to an earlier one): golang-migrate records
-- applied versions and never re-runs them, so amending an applied migration would
-- silently skip databases that already ran it.
CREATE INDEX IF NOT EXISTS idx_campaigns_stuck_claims
    ON campaigns (created_at)
    WHERE status = 'pending';
