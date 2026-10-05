-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the table discards the SAVED REPORTS only. They are a cache of data the ad
-- platform owns: nothing is lost that a fresh report cannot rebuild. After a re-migration
-- the monitor serves no metrics for a report-backed platform until its next report
-- finishes (minutes), and any report pending at the time is simply never collected.
DROP TABLE IF EXISTS account_monitor_reports;
