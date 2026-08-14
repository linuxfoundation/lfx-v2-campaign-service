-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop the narrower (brief_id, platform) index, superseded by 000022's
-- (brief_id, platform, variant).
--
-- SAFE ONLY BECAUSE 000023 RAN FIRST. That migration verifies the replacement is
-- present, VALID, unique, keyed on exactly those three columns and partial on
-- status <> 'deleted' -- and raises otherwise. golang-migrate applies versions in
-- order and stops on failure, so this file is reached only when that held.
--
-- Bare and alone: DROP INDEX CONCURRENTLY cannot run inside a transaction, and a
-- DO $$ guard is one -- which is why the check could not live in this file. See
-- 000013 for why CONCURRENTLY matters here at all: migrations run during a rolling
-- restart while other replicas are claiming dispatches, and a blocking drop would
-- stall a claim mid-flight.

DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_brief_platform_live;
