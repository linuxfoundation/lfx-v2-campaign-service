-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- A second report KIND in keyword_insight_reports (000038): the Microsoft age/gender audience
-- read (get-microsoft-ads-audience, LFXV2-2665), served from a saved asynchronous
-- AgeGenderAudienceReportRequest by exactly the keyword read's mechanism -- a READY half served
-- while a PENDING half builds, completed or failed by a compare-and-set on the pending report id,
-- each half recording the campaign scope it was built for. Kinds:
--
--   keywords   -- the unprefixed columns, unchanged since 000038 (KeywordPerformanceReportRequest)
--   age_gender -- the age_gender_-prefixed columns added here (AgeGenderAudienceReportRequest)
--
-- WHY A COLUMN SET PER KIND, NOT A report_kind ROW DISCRIMINATOR. A discriminator would have to
-- join the PRIMARY KEY (one row per kind per key), and replacing the primary key is not an
-- expansion: the N-1 binary's MarkKeywordReportPending upserts ON CONFLICT (project_id,
-- platform, account_id, report_window), which needs a unique index on exactly those four
-- columns. Keep that index and two kinds cannot share a key; drop it and the N-1 upsert fails
-- for the length of the rollout. One row per key with a nullable column set per kind keeps the
-- key, every N-1 statement, and the keyword columns' constraints exactly as they are.
--
-- KIND SEPARATION is structural: the audience statements name only age_gender_* columns and the
-- keyword statements (unchanged) only the unprefixed ones, so a keyword report can never be read,
-- completed, failed or served as an audience report, or the reverse. A row created by one kind
-- has the other kind's columns all NULL, which both readers treat as "nothing saved".
--
-- EXPAND-only: nullable columns with no default (metadata-only ADD COLUMN) and CHECK constraints
-- every existing row satisfies (all-NULL halves). The N-1 binary never names these columns; its
-- INSERT leaves them NULL and its UPDATEs leave them untouched, so a rolling deploy is safe in
-- both directions. No index (every statement addresses one row by its full primary key) and no
-- requiredIndexes entry, as for 000038.
ALTER TABLE keyword_insight_reports
    -- READY half of the age_gender kind: the last report that finished.
    ADD COLUMN IF NOT EXISTS age_gender_ready_report_id      TEXT,
    -- JSONB array of model.AudienceReportRow (one per campaign, age group and gender). An EMPTY
    -- array is a finished report in which none of the scoped campaigns served; NULL means no
    -- age/gender report has finished.
    ADD COLUMN IF NOT EXISTS age_gender_ready_rows           JSONB,
    ADD COLUMN IF NOT EXISTS age_gender_ready_partial        BOOLEAN,
    ADD COLUMN IF NOT EXISTS age_gender_ready_campaign_ids   TEXT[],
    ADD COLUMN IF NOT EXISTS age_gender_ready_window_start   DATE,
    ADD COLUMN IF NOT EXISTS age_gender_ready_window_end     DATE,
    -- When the report was REQUESTED, as ready_as_of.
    ADD COLUMN IF NOT EXISTS age_gender_ready_as_of          TIMESTAMPTZ,
    -- PENDING half of the age_gender kind.
    ADD COLUMN IF NOT EXISTS age_gender_pending_report_id    TEXT,
    ADD COLUMN IF NOT EXISTS age_gender_pending_campaign_ids TEXT[],
    ADD COLUMN IF NOT EXISTS age_gender_pending_window_start DATE,
    ADD COLUMN IF NOT EXISTS age_gender_pending_window_end   DATE,
    ADD COLUMN IF NOT EXISTS age_gender_pending_submitted_at TIMESTAMPTZ,
    -- The most recent age/gender report the platform failed or this service abandoned.
    ADD COLUMN IF NOT EXISTS age_gender_last_failure         TEXT,
    ADD COLUMN IF NOT EXISTS age_gender_last_failure_at      TIMESTAMPTZ;

-- Each half ALL-OR-NOTHING, exactly as 000038's constraints on the keyword halves: the reader
-- decides whether a half exists from its report id alone.
ALTER TABLE keyword_insight_reports
    DROP CONSTRAINT IF EXISTS keyword_insight_reports_age_gender_ready_whole,
    ADD CONSTRAINT keyword_insight_reports_age_gender_ready_whole CHECK (
        (age_gender_ready_report_id IS NULL AND age_gender_ready_rows IS NULL
         AND age_gender_ready_partial IS NULL AND age_gender_ready_campaign_ids IS NULL
         AND age_gender_ready_window_start IS NULL AND age_gender_ready_window_end IS NULL
         AND age_gender_ready_as_of IS NULL)
        OR
        (age_gender_ready_report_id IS NOT NULL AND age_gender_ready_rows IS NOT NULL
         AND age_gender_ready_partial IS NOT NULL AND age_gender_ready_campaign_ids IS NOT NULL
         AND age_gender_ready_window_start IS NOT NULL AND age_gender_ready_window_end IS NOT NULL
         AND age_gender_ready_as_of IS NOT NULL)
    ),
    DROP CONSTRAINT IF EXISTS keyword_insight_reports_age_gender_pending_whole,
    ADD CONSTRAINT keyword_insight_reports_age_gender_pending_whole CHECK (
        (age_gender_pending_report_id IS NULL AND age_gender_pending_campaign_ids IS NULL
         AND age_gender_pending_window_start IS NULL AND age_gender_pending_window_end IS NULL
         AND age_gender_pending_submitted_at IS NULL)
        OR
        (age_gender_pending_report_id IS NOT NULL AND age_gender_pending_campaign_ids IS NOT NULL
         AND age_gender_pending_window_start IS NOT NULL AND age_gender_pending_window_end IS NOT NULL
         AND age_gender_pending_submitted_at IS NOT NULL)
    ),
    DROP CONSTRAINT IF EXISTS keyword_insight_reports_age_gender_ready_rows_array,
    ADD CONSTRAINT keyword_insight_reports_age_gender_ready_rows_array CHECK (
        age_gender_ready_rows IS NULL OR jsonb_typeof(age_gender_ready_rows) = 'array'
    );

-- GROWTH: unchanged in row count (still one row per project, platform, account and window ever
-- read); each row now also holds at most one age/gender report, bounded by the scoped campaigns
-- times Microsoft's six age groups and its gender values, and upstream by the 8 MiB download cap.
