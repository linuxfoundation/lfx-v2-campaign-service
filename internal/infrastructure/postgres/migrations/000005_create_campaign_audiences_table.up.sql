-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- campaign_audiences: a built marketing audience, subordinate to a brief. It stores
-- a POINTER + provenance to the audience that physically lives in the platform (a
-- HubSpot master contact list) — NOT the audience contents. This makes a built
-- audience a first-class, inspectable, reusable, versioned LFX resource (the "B2"
-- decision, epic LFXV2-2770) rather than a throwaway side-effect of a send.
--
-- hierarchy: Project -> Brief -> Audiences (one brief can have several built
-- audiences over time / per platform).
CREATE TABLE IF NOT EXISTS campaign_audiences (
    id                     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id             UUID        NOT NULL,
    brief_id               UUID        NOT NULL REFERENCES campaign_briefs(id),
    platform               TEXT        NOT NULL,          -- 'hubspot'
    -- The pointer to the real audience in the platform (a HubSpot master list id).
    platform_master_list_id TEXT,
    -- The suppression list ids applied when the master was built (platform ids).
    suppression_list_ids   JSONB,
    -- Human-readable provenance: how it was built (past events, geo, topic, etc.).
    inclusion_summary      TEXT,
    status                 TEXT        NOT NULL DEFAULT 'building'
                           CHECK (status IN ('building','built','failed')),
    version                BIGINT      NOT NULL DEFAULT 1,
    created_by             JSONB,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- brief_id is the primary lookup axis (list a brief's audiences); project_id is the
-- tenant-scoping predicate on every read. Neither is the leftmost column of a unique
-- index (there is no natural uniqueness — a brief may have many audiences), so both
-- get their own index.
CREATE INDEX IF NOT EXISTS idx_campaign_audiences_brief_id ON campaign_audiences (brief_id);
CREATE INDEX IF NOT EXISTS idx_campaign_audiences_project_id ON campaign_audiences (project_id);
