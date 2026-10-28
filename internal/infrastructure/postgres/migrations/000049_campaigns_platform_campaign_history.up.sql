-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Monitor ownership checks also search deleted bindings. The per-provider unique
-- indexes cover only live rows, so they cannot support that history lookup.
-- This non-unique, all-status index changes no binding/soft-delete semantics.
--
-- One statement, no transaction, and no IF NOT EXISTS: a failed concurrent build
-- leaves an INVALID index. Drop that leftover before forcing version 48 and retrying,
-- rather than recording a clean migration over an index PostgreSQL cannot use.
CREATE INDEX CONCURRENTLY idx_campaigns_platform_campaign_history
    ON campaigns (platform, platform_campaign_id);
