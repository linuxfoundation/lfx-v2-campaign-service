-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the table discards SAVED KEYWORD REPORTS only: a cache of data the ad platform owns,
-- rebuilt by the next report. After a re-migration the Microsoft keyword read serves no rows
-- until its next report finishes (minutes); a report pending at the time is never collected.
DROP TABLE IF EXISTS keyword_insight_reports;
