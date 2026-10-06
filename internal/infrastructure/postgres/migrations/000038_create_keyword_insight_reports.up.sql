-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- keyword_insight_reports: the saved state behind the project-scoped keyword read for a
-- platform whose keyword performance cannot be read inside one request (Microsoft Advertising's
-- KeywordPerformanceReportRequest, built asynchronously in minutes against a 20-second call
-- budget). The mechanism is account_monitor_reports' (000035) exactly -- a READY half served
-- while a PENDING half builds, completed or failed by a compare-and-set on pending_report_id --
-- and the reasoning recorded there applies unchanged. See model.KeywordReportSnapshot.
--
-- WHY A SIBLING TABLE, NOT A "kind" COLUMN ON account_monitor_reports. Three things differ, and
-- none can be added to 000035's table expand-only:
--   * The KEY. The keyword read is keyed by the reporting window ('last_30_days', 'this_month',
--     ...), not by an integer day count; account_monitor_reports' days column is NOT NULL with
--     CHECK (days BETWEEN 7 AND 90) and is part of its PRIMARY KEY. Adding a kind to that key
--     means replacing the primary key, which the N-1 binary's ON CONFLICT (project_id, platform,
--     account_id, days) depends on -- a contract change, not an expansion.
--   * The SCOPE. A keyword report covers the project's OWN campaigns, a set that changes as
--     campaigns are dispatched or deleted, so each half records the campaign ids it was built
--     for (ready_campaign_ids / pending_campaign_ids). The account report has no such column.
--   * The ROWS. ready_rows holds model.KeywordReportRow, not model.AccountReportRow; sharing a
--     column would make every reader of one decode the other's rows on a key mix-up.
-- A new table is purely additive: nothing in the N-1 binary reads or writes it.
--
-- WHY NO FOREIGN KEY, and NO requiredIndexes ENTRY: as for 000035 -- the row is a cache keyed
-- by the platform's identity, and the only uniqueness relied on is the PRIMARY KEY itself.
CREATE TABLE IF NOT EXISTS keyword_insight_reports (
    -- TEXT, matching every other project-scoped table: project ids are slugs as often as UUIDs.
    project_id           TEXT        NOT NULL,
    -- model.Provider ('microsoft-ads' today). Not constrained to one value, as in 000035.
    platform             TEXT        NOT NULL,
    -- The ad account the project's own connection is bound to when the report was submitted.
    -- In the key so a re-pointed connection never serves the previous account's report.
    account_id           TEXT        NOT NULL,
    -- model.MetricsWindow. Mirrors model.IsValidMetricsWindow; the service refuses anything else
    -- with a 400 first, and the CHECK makes the vocabulary an invariant for any other writer.
    report_window        TEXT        NOT NULL CHECK (report_window IN
        ('today', 'yesterday', 'last_7_days', 'last_14_days', 'last_30_days', 'this_month', 'last_month')),

    -- READY half: the last report that finished.
    ready_report_id      TEXT,
    -- JSONB array of model.KeywordReportRow. An EMPTY array is a finished report in which none
    -- of the scoped campaigns' keywords served; NULL means no report has finished.
    ready_rows           JSONB,
    ready_partial        BOOLEAN,
    -- The platform campaign ids the ready report was scoped to. The read serves it only while
    -- it covers every campaign the project now owns.
    ready_campaign_ids   TEXT[],
    ready_window_start   DATE,
    ready_window_end     DATE,
    -- When the report was REQUESTED (the point in time its data describes), as in 000035.
    ready_as_of          TIMESTAMPTZ,

    -- PENDING half: the report submitted and not yet collected.
    pending_report_id    TEXT,
    pending_campaign_ids TEXT[],
    pending_window_start DATE,
    pending_window_end   DATE,
    pending_submitted_at TIMESTAMPTZ,

    -- The most recent report the platform failed or this service abandoned, for operators.
    last_failure         TEXT,
    last_failure_at      TIMESTAMPTZ,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (project_id, platform, account_id, report_window),

    -- Each half is ALL-OR-NOTHING, for 000035's reason: the reader decides whether a half exists
    -- from its report id alone.
    CONSTRAINT keyword_insight_reports_ready_whole CHECK (
        (ready_report_id IS NULL AND ready_rows IS NULL AND ready_partial IS NULL
         AND ready_campaign_ids IS NULL AND ready_window_start IS NULL
         AND ready_window_end IS NULL AND ready_as_of IS NULL)
        OR
        (ready_report_id IS NOT NULL AND ready_rows IS NOT NULL AND ready_partial IS NOT NULL
         AND ready_campaign_ids IS NOT NULL AND ready_window_start IS NOT NULL
         AND ready_window_end IS NOT NULL AND ready_as_of IS NOT NULL)
    ),
    CONSTRAINT keyword_insight_reports_pending_whole CHECK (
        (pending_report_id IS NULL AND pending_campaign_ids IS NULL AND pending_window_start IS NULL
         AND pending_window_end IS NULL AND pending_submitted_at IS NULL)
        OR
        (pending_report_id IS NOT NULL AND pending_campaign_ids IS NOT NULL
         AND pending_window_start IS NOT NULL AND pending_window_end IS NOT NULL
         AND pending_submitted_at IS NOT NULL)
    ),
    CONSTRAINT keyword_insight_reports_ready_rows_array CHECK (
        ready_rows IS NULL OR jsonb_typeof(ready_rows) = 'array'
    )
);
-- No secondary index: every statement addresses one row by its full primary key.
--
-- GROWTH: one row per (project, platform, account, window) ever read, overwritten in place --
-- bounded by projects times the seven windows, not by traffic. Each ready_rows holds at most
-- the scoped campaigns' keywords, bounded upstream by the 8 MiB report download cap.
