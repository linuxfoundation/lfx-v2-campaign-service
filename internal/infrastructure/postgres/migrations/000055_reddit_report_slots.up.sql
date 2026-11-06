-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Add reddit_report_slots: the Reddit reporting budget's SHARED ledger (#380). One row per
-- reporting attempt (POST /ad_accounts/{id}/reports) any replica took a slot for, so the
-- account-wide budget — 45 of Reddit's 60 requests per ad account per minute, 24 of them for the
-- insight reads — holds however many replicas run. Before this each process kept its own ledger in
-- memory, and N replicas could spend N × 45.
--
-- WRITTEN ONLY by postgres.RedditReportLedger: Take counts the account's rows inside the window
-- and inserts one when there is room, under a per-account transaction advisory lock, stamping
-- taken_at from the database's clock (clock_timestamp()) so replicas with skewed clocks agree on
-- the window. Release deletes a row by id (an attempt proven NOT sent). Expired rows are pruned
-- by every Take, so the table holds roughly one window of attempts.
--
-- No foreign key: ad_account_id is Reddit's id, not a row in this database, and the ledger must
-- count an account's attempts whichever connection made them.
--
-- EXPAND-only: a new table no existing code reads. The N-1 binary never touches it (its ledger is
-- in memory), so a rolling deploy is safe in both directions — while N-1 and N pods overlap, the
-- N-1 pod's attempts are simply not counted here, as before.

CREATE TABLE IF NOT EXISTS reddit_report_slots (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ad_account_id TEXT NOT NULL,
    insights BOOLEAN NOT NULL,
    taken_at TIMESTAMPTZ NOT NULL
);

-- Take's count is per account inside the window.
CREATE INDEX IF NOT EXISTS reddit_report_slots_account_taken_at_idx
    ON reddit_report_slots (ad_account_id, taken_at);

-- Take's prune is across all accounts, by age.
CREATE INDEX IF NOT EXISTS reddit_report_slots_taken_at_idx
    ON reddit_report_slots (taken_at);
