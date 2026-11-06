-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop reddit_report_slots, the Reddit reporting budget's shared ledger.
--
-- Always applies and loses nothing durable: every row is a slot that expires within a minute.
-- A binary that still binds postgres.RedditReportLedger then fails every Reddit report closed
-- (the table is gone), so roll the binary back first.

DROP TABLE IF EXISTS reddit_report_slots;
