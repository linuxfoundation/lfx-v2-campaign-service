-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting NARROWS the key, so it can fail -- and failing is the correct behaviour rather than
-- a defect to work around. Once an event holds both a paid and an email brief, or an email
-- series of more than one stage, those rows have no representation under
-- `(project_id, event_slug)`: restoring that index would have to choose which briefs to destroy.
-- Postgres refuses instead, and the operator resolves it deliberately by archiving the rows they
-- do not want before retrying.
--
-- WRAPPED IN A TRANSACTION, and that is load-bearing rather than decorative. golang-migrate does
-- not wrap a migration's statements for us, so an earlier revision -- which simply ordered the
-- index restore before the column drops -- left a database in a state worse than either endpoint
-- when it failed: verified by running it against a seeded database, where the index creation
-- errored on the duplicate key and the two ALTER TABLEs then ran anyway, dropping
-- `delivery_type` and `stage` while the duplicate rows they distinguished remained. The table
-- was left with data it could no longer tell apart and neither index in place.
--
-- With the transaction, a failed revert is a no-op: the columns and the wide index survive
-- exactly as they were, and the operator sees the duplicate-key error naming the event to
-- resolve.

BEGIN;

DROP INDEX IF EXISTS uq_campaign_briefs_project_event_delivery_stage;

CREATE UNIQUE INDEX IF NOT EXISTS uq_campaign_briefs_project_event
    ON campaign_briefs (project_id, event_slug)
    WHERE status <> 'archived';

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

-- The pair constraint goes with them. It references BOTH columns, so leaving it would make the
-- column drops below fail and take this transaction with them.
ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_stage_pair_valid;

ALTER TABLE campaign_briefs
    DROP COLUMN IF EXISTS delivery_type;

ALTER TABLE campaign_briefs
    DROP COLUMN IF EXISTS stage;

COMMIT;
