-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- One live row per Meta campaign (LFXV2-2665): one upstream Meta campaign cannot be bound to two
-- briefs in any project.
--
-- GLOBAL key, because Meta campaign ids are globally unique: a campaign is a Graph API node, and
-- "A node is an individual object with a unique ID" (https://developers.facebook.com/docs/graph-api/overview);
-- the adapter reads it as GET /{campaign_id} with no account in the path. The same id in two
-- projects IS the same paid campaign, so the key needs no account.
--
-- A SEPARATE index per provider, never a wider 000020/000042 predicate — see 000020 and the
-- LFXV2-3056 log entry: whether a global key is right is a per-provider question.
--
-- AdoptCampaign and UpsertCampaign classify its 23505 as ErrPlatformCampaignAlreadyBound
-- (isPlatformCampaignBindingViolation): adoption answers the "already bound to another brief"
-- 409, and a dispatch that resolved to an already-bound campaign is reported as that named
-- conflict with its claim retained.
--
-- status <> 'deleted' and platform_campaign_id IS NOT NULL for 000020's reasons.
--
-- CONCURRENTLY, one statement per file, no transaction, and NO `IF NOT EXISTS` — 000020's
-- reasons: a retry over an INVALID leftover must fail with 42P07, not record this version clean
-- over an index that enforces nothing.
--
-- DEPLOY RISK: if live rows already bind one upstream campaign twice, the build fails, Migrate
-- leaves version 43 recorded DIRTY, and every later boot refuses with ErrDirty. Recovery, in order:
--   1. fix the duplicate binding (soft-delete one of the rows: UPDATE campaigns SET
--      status = 'deleted' WHERE id = '<row id>');
--   2. DROP INDEX CONCURRENTLY uq_campaigns_meta_campaign_live;  -- the INVALID leftover
--   3. force the version back to its predecessor with the golang-migrate CLI against the same
--      database (default schema_migrations table):
--        migrate -path internal/infrastructure/postgres/migrations -database "$DATABASE_URL" force 42
--      (forcing back ONE version replays only this single-statement migration — see
--      docs/knowledge/code/internal-infrastructure-postgres.md on why longer forces are unsafe);
--   4. re-run: restart the service, whose boot Migrate applies 43 again.
CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_meta_campaign_live
    ON campaigns (platform, platform_campaign_id)
    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'meta-ads';
