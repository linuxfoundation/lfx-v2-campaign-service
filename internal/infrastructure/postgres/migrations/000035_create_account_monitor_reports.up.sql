-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- account_monitor_reports: the saved state behind the ASYNCHRONOUS half of the account
-- monitor, for a platform whose delivery metrics cannot be read inside one request.
--
-- WHY A TABLE. Microsoft Advertising serves performance data only through its Reporting
-- service -- submit, poll, download -- and documents that reports "complete within minutes",
-- to be polled at 2-15 minute intervals. The monitor read runs inside a 20-second call
-- budget, so a synchronous read would essentially never see a finished report. Instead a
-- request reads the campaign list live, serves metrics from the last report that FINISHED,
-- and checks on (or submits) the next one without waiting for it. That state has to outlive
-- the request and be visible from every replica (the pod that submits a report is routinely
-- not the pod that collects it), which is what this table is. The full reasoning lives on
-- model.AccountReportSnapshot in internal/domain/model/account_report.go.
--
-- WHY TWO HALVES IN ONE ROW. ready_* is the last report that finished and is what a request
-- serves, possibly stale; pending_* is the report currently building on the platform. They
-- are independent on purpose: a newer report can be building while an older one is still
-- being served, and collapsing them into one status column would force a choice between
-- serving nothing while a report builds and never noticing a newer one. Completing or
-- failing a report is a compare-and-set on pending_report_id (see account_report_repo.go),
-- so a request that collected an OLDER report cannot clear a newer submission's marker.
--
-- WHY days IS IN THE KEY. A 7-day and a 30-day view are different reports over different
-- windows, not one report read two ways; keying without days would let one view's rows be
-- served as the other's.
--
-- WHY NO FOREIGN KEY. There is no single parent to reference: connections live in one table
-- per provider (microsoft_ads_connections, ...), and platform is a column here. The row is
-- keyed by (project, platform, account) as the platform reports it, not by a connection row
-- id: the ad account is the platform's identity, and a
-- connection can be dropped and recreated for the same account without the saved report
-- becoming wrong. The table is a CACHE of platform data -- nothing here is a system of
-- record -- so an orphaned row costs a stale read at worst, never a wrong write elsewhere.
--
-- NO requiredIndexes ENTRY. The only uniqueness this table relies on is its PRIMARY KEY,
-- which is a constraint the table cannot exist without, not a unique index standing in for
-- one; requiredIndexes guards the latter kind (see migrations/README.md).
--
-- EXPAND-ONLY. This is a NEW table: nothing in the N-1 binary reads or writes it, so the
-- N-1 binary runs unchanged against this schema during a rolling deploy.
CREATE TABLE IF NOT EXISTS account_monitor_reports (
    -- project_id is TEXT, not UUID, matching every other project-scoped table here: project
    -- ids are slugs like 'cncf' as often as they are UUIDs.
    project_id           TEXT        NOT NULL,
    -- platform is model.Provider ('microsoft-ads' today). Deliberately NOT constrained to one
    -- value: the store is platform-neutral, and a second asynchronous-reporting platform
    -- reuses it without a migration.
    platform             TEXT        NOT NULL,
    account_id           TEXT        NOT NULL,
    -- Mirrors domain.MonitorDaysMin / MonitorDaysMax (internal/domain/errors.go). The service
    -- rejects anything outside the range with a 400 before it gets here; the CHECK makes the
    -- range an invariant of the table for any other writer.
    days                 INTEGER     NOT NULL CHECK (days BETWEEN 7 AND 90),

    -- READY half: the last report that finished.
    ready_report_id      TEXT,
    -- ready_rows is a JSONB array of model.AccountReportRow. An EMPTY array is meaningful --
    -- a finished report in which no campaign served -- and is distinct from NULL, which
    -- means no report has finished at all.
    ready_rows           JSONB,
    -- ready_partial: the platform flagged the window's last day as still aggregating.
    ready_partial        BOOLEAN,
    -- Report windows are calendar dates (UTC); a time of day would claim a precision the
    -- platform's daily aggregation does not have.
    ready_window_start   DATE,
    ready_window_end     DATE,
    -- ready_as_of: when the report was REQUESTED from the platform, which is the point in
    -- time its data describes -- not when this service happened to collect it, which can be
    -- an hour later and would overstate freshness by that much.
    ready_as_of          TIMESTAMPTZ,

    -- PENDING half: the report submitted and not yet collected.
    pending_report_id    TEXT,
    pending_window_start DATE,
    pending_window_end   DATE,
    pending_submitted_at TIMESTAMPTZ,

    -- The most recent report the platform failed or this service abandoned, for operators.
    -- Informational only: it never blocks a fresh submission.
    last_failure         TEXT,
    last_failure_at      TIMESTAMPTZ,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (project_id, platform, account_id, days),

    -- Each half is ALL-OR-NOTHING. The reader decides whether a half exists from its report
    -- id alone, so a half with an id and a missing window (or rows) would decode as a report
    -- that is present but describes nothing. ready_partial is part of the ready half too: a
    -- NULL there would silently read as "not partial".
    CONSTRAINT account_monitor_reports_ready_whole CHECK (
        (ready_report_id IS NULL AND ready_rows IS NULL AND ready_partial IS NULL
         AND ready_window_start IS NULL AND ready_window_end IS NULL
         AND ready_as_of IS NULL)
        OR
        (ready_report_id IS NOT NULL AND ready_rows IS NOT NULL AND ready_partial IS NOT NULL
         AND ready_window_start IS NOT NULL AND ready_window_end IS NOT NULL
         AND ready_as_of IS NOT NULL)
    ),
    CONSTRAINT account_monitor_reports_pending_whole CHECK (
        (pending_report_id IS NULL AND pending_window_start IS NULL
         AND pending_window_end IS NULL AND pending_submitted_at IS NULL)
        OR
        (pending_report_id IS NOT NULL AND pending_window_start IS NOT NULL
         AND pending_window_end IS NOT NULL AND pending_submitted_at IS NOT NULL)
    ),
    -- The reader decodes ready_rows into a Go slice; an object or scalar there would fail
    -- that decode on every read of the key. Refuse it at the write instead.
    CONSTRAINT account_monitor_reports_ready_rows_array CHECK (
        ready_rows IS NULL OR jsonb_typeof(ready_rows) = 'array'
    )
);
-- No secondary index: every read and write addresses one row by its full primary key.
--
-- GROWTH: one row per (project, platform, account, days) ever monitored, overwritten in
-- place rather than appended to, so the table is bounded by the number of monitored
-- accounts times the handful of day windows the UI offers -- not by request traffic.
