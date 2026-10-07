-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the age_gender columns discards SAVED AGE/GENDER REPORTS only: a cache of data the
-- ad platform owns. The keyword kind's columns are untouched. A binary that still serves the
-- Microsoft audience read fails its saved-report statements until it is rolled back too, so
-- roll the binary back first.
ALTER TABLE keyword_insight_reports
    DROP CONSTRAINT IF EXISTS keyword_insight_reports_age_gender_ready_rows_array,
    DROP CONSTRAINT IF EXISTS keyword_insight_reports_age_gender_pending_whole,
    DROP CONSTRAINT IF EXISTS keyword_insight_reports_age_gender_ready_whole,
    DROP COLUMN IF EXISTS age_gender_last_failure_at,
    DROP COLUMN IF EXISTS age_gender_last_failure,
    DROP COLUMN IF EXISTS age_gender_pending_submitted_at,
    DROP COLUMN IF EXISTS age_gender_pending_window_end,
    DROP COLUMN IF EXISTS age_gender_pending_window_start,
    DROP COLUMN IF EXISTS age_gender_pending_campaign_ids,
    DROP COLUMN IF EXISTS age_gender_pending_report_id,
    DROP COLUMN IF EXISTS age_gender_ready_as_of,
    DROP COLUMN IF EXISTS age_gender_ready_window_end,
    DROP COLUMN IF EXISTS age_gender_ready_window_start,
    DROP COLUMN IF EXISTS age_gender_ready_campaign_ids,
    DROP COLUMN IF EXISTS age_gender_ready_partial,
    DROP COLUMN IF EXISTS age_gender_ready_rows,
    DROP COLUMN IF EXISTS age_gender_ready_report_id;
