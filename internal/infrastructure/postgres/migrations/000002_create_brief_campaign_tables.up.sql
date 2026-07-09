-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Brief, campaign, and async-job tables. Briefs and campaigns ARE indexed into
-- the Query Service (unlike connections), so history/lists come from there; no
-- version/audit tables live here. Hierarchy: Project -> Brief -> Campaigns.

CREATE TABLE IF NOT EXISTS campaign_briefs (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    UUID        NOT NULL,
    program_type  TEXT        NOT NULL
                  CHECK (program_type IN ('events','education','membership')),
    event_slug    TEXT        NOT NULL,
    url           TEXT,
    platforms     JSONB,
    event_details JSONB,
    copy          JSONB,
    keywords      JSONB,
    targeting     JSONB,
    status        TEXT        NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','approved','archived')),
    version       BIGINT      NOT NULL DEFAULT 1,
    approved_by   JSONB,
    approved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, event_slug)
);
-- No standalone project_id index: UNIQUE (project_id, event_slug) already
-- indexes project_id as its leftmost column, covering project_id-only lookups.

CREATE TABLE IF NOT EXISTS campaign_jobs (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    brief_id   UUID        NOT NULL REFERENCES campaign_briefs(id),
    status     TEXT        NOT NULL DEFAULT 'queued'
               CHECK (status IN ('queued','running','succeeded','partial','failed')),
    result     JSONB,
    error      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_campaign_jobs_brief_id ON campaign_jobs (brief_id);

CREATE TABLE IF NOT EXISTS campaigns (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id           UUID        NOT NULL,
    brief_id             UUID        NOT NULL REFERENCES campaign_briefs(id),
    job_id               UUID,                          -- soft ref (jobs are ephemeral)
    platform             TEXT        NOT NULL,
    platform_campaign_id TEXT,
    campaign_name        TEXT        NOT NULL,
    status               TEXT        NOT NULL,
    budget_amount        NUMERIC(14,2),
    budget_type          TEXT        CHECK (budget_type IN ('daily','lifetime')),
    start_date           DATE,
    end_date             DATE,
    config_snapshot      JSONB,
    result               JSONB,
    version              BIGINT      NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (brief_id, platform)
);
-- No standalone brief_id index: UNIQUE (brief_id, platform) already indexes
-- brief_id as its leftmost column. project_id is NOT a leftmost column of any
-- unique index, so it keeps its own index.
CREATE INDEX IF NOT EXISTS idx_campaigns_project_id ON campaigns (project_id);
