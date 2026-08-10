-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Index the tombstone probe ConnectionRepo.Disconnected runs.
--
-- Every connection table already indexes project_id, but only under
-- `WHERE status <> 'deleted'` (000001) — a predicate that excludes exactly the rows this
-- probe reads. So the probe has no usable index at all, and it runs on the hottest miss
-- path there is: every dispatch for a project with no connection of its own asks
-- "did this project disconnect?" before it is allowed to use the system account. Without
-- these, that question is a sequential scan of the whole provider table, and it gets
-- slower for every project that ever connects.
--
-- The predicate is the mirror of the existing one, so the two indexes partition the table
-- between them and neither pays for the other's rows. Deleted rows are the small side, but
-- NOT one-per-project: the uniqueness in 000001 is partial (`WHERE status <> 'deleted'`),
-- so it constrains live rows only and a project that connects and disconnects repeatedly
-- accumulates one tombstone per cycle. Small is a statement about the RATE — disconnecting
-- is a deliberate operator action, not traffic — not a per-project bound.
--
-- Plain CREATE INDEX, not CONCURRENTLY, and that is a departure from 000008 worth stating.
-- 000008 indexes `campaigns`, which grows with every dispatch forever and is written by
-- every replica on a 5-minute sweep; there a build-long ACCESS EXCLUSIVE lock can stall a
-- claim mid-flight. These tables are the opposite shape: at most one LIVE row per (project,
-- provider) plus the disconnect tombstones, written only when an operator connects,
-- rotates or disconnects. 000001 built the mirror indexes on these same six tables with a
-- plain CREATE, and 000010/000011/000012 do the same on index_outbox; CONCURRENTLY in this
-- repo is the exception for `campaigns`, not the house rule. It is also not free: a failed
-- concurrent build leaves an INVALID index that `IF NOT EXISTS` then silently skips, which
-- is why 000008 needed the 000009 recovery migration. Taking that on six times over, to
-- protect a millisecond-scale build on a table this size, buys less than it costs.
--
-- Only the six PAID-ADS tables are covered, because only they are reached. The probe is
-- called from credsSource.systemFallback, below a `provider.IsPaidAds()` gate — the system
-- account is an ad-ACCOUNT fallback, and HubSpot (ChannelEmail) never falls back, so
-- hubspot_connections would carry an index nothing queries and every write would pay for
-- it. If that gate ever widens, this migration widens with it.

CREATE INDEX IF NOT EXISTS idx_google_ads_connections_project_deleted    ON google_ads_connections    (project_id) WHERE status = 'deleted';
CREATE INDEX IF NOT EXISTS idx_linkedin_ads_connections_project_deleted  ON linkedin_ads_connections  (project_id) WHERE status = 'deleted';
CREATE INDEX IF NOT EXISTS idx_meta_ads_connections_project_deleted      ON meta_ads_connections      (project_id) WHERE status = 'deleted';
CREATE INDEX IF NOT EXISTS idx_reddit_ads_connections_project_deleted    ON reddit_ads_connections    (project_id) WHERE status = 'deleted';
CREATE INDEX IF NOT EXISTS idx_twitter_ads_connections_project_deleted   ON twitter_ads_connections   (project_id) WHERE status = 'deleted';
CREATE INDEX IF NOT EXISTS idx_microsoft_ads_connections_project_deleted ON microsoft_ads_connections (project_id) WHERE status = 'deleted';
