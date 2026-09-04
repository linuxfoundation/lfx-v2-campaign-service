-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting NARROWS the key, so it can fail -- and failing is the correct behaviour rather than
-- a defect to work around. Once an event holds both a paid and an email brief, or an email
-- series of more than one stage, those rows have no representation under
-- `(project_id, event_slug)`: restoring that index would have to choose which briefs to destroy.
-- Postgres refuses instead, and the operator resolves it deliberately by archiving the rows they
-- do not want before retrying.
--
-- WRAPPED IN A TRANSACTION, as a defensive guarantee rather than as a fix for a live hazard.
-- Under THIS repository's configuration the batch is already atomic: `pgxURL` (pool.go) rewrites
-- only the URL scheme and never sets `x-multi-statement`, so the pgx5 driver submits the whole
-- file in one ExecContext and PostgreSQL wraps a multi-statement simple query in an IMPLICIT
-- transaction. The migrations README states the same thing at line 59, where it explains why
-- CREATE INDEX CONCURRENTLY must live alone in its own file.
--
-- An earlier revision of this comment claimed the opposite -- that golang-migrate ran the
-- statements individually, so a failing index restore would let the column drops run anyway and
-- strand duplicate rows with their discriminator gone. That failure mode is not reachable here,
-- and the claim was wrong: it was reasoned from the fix working rather than checked against the
-- driver's configuration.
--
-- The BEGIN/COMMIT stays for two reasons that do hold. It survives someone enabling
-- `x-multi-statement` later -- the flag exists precisely so a file CAN be run
-- statement-by-statement -- and it states the atomicity requirement HERE rather than leaving it
-- to a driver default two layers away.
--
-- Either way a failed revert is a no-op: the columns and the wide index survive exactly as they
-- were, and the operator sees the duplicate-key error naming the event to resolve.

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

-- REFUSE while any live brief carries a non-paid identity, not merely when two rows collide.
--
-- The narrow index above only catches CARDINALITY: an event with one paid brief and one email
-- brief trips it, but an event with a SINGLE live email brief does not -- there is nothing for it
-- to collide with. The drops below would then erase that row's delivery_type and stage, and the
-- pre-000030 service, which reads `WHERE project_id=$1 AND event_slug=$2`, would hand back its
-- EMAIL content as the event's paid brief: RSA headlines and a keyword list that belong to a
-- different channel, with nothing left in the row to say so. Verified against a seeded database.
--
-- So the guard is on the IDENTITY, not the collision. A `DO` block, because that is what lets a
-- plain SQL migration raise a message an operator can act on rather than a bare constraint error;
-- it aborts the surrounding transaction, so a refused revert is a no-op exactly as a duplicate-key
-- failure is.
--
-- To proceed, the operator must first archive or migrate every non-paid brief. That is a decision
-- about content -- which sends are worth keeping -- and it is not one a rollback should make
-- silently on their behalf.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM campaign_briefs
        WHERE status <> 'archived'
          AND (delivery_type <> 'paid-marketing' OR stage <> '')
    ) THEN
        RAISE EXCEPTION
            'cannot revert 000030: live briefs exist with a non-paid delivery_type or a stage. '
            'Dropping these columns would leave that content indistinguishable from the paid '
            'brief. Archive or migrate those rows first.';
    END IF;
END $$;

ALTER TABLE campaign_briefs
    DROP COLUMN IF EXISTS delivery_type;

ALTER TABLE campaign_briefs
    DROP COLUMN IF EXISTS stage;

COMMIT;
