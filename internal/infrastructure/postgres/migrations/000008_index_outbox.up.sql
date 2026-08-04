-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Transactional outbox for Query Service index messages.
--
-- Publishing after commit is at-most-once: the process can die between the two, and core NATS
-- has no replay. Nothing repairs that — a terminal write (archiving a brief) has no "next
-- write", and a created-then-never-edited campaign may never be written again, so the index can
-- stay permanently stale or missing the only document backing lists and history.
--
-- A row here is written in the SAME transaction as the resource, so it commits if and only if
-- the resource does. A relay then publishes it and marks it done: delivery becomes recoverable
-- independently of the write, which is the property the direct publish cannot provide.
CREATE TABLE IF NOT EXISTS index_outbox (
    id           BIGSERIAL    PRIMARY KEY,
    -- The NATS subject's object type (campaign_brief / campaign).
    object_type  TEXT        NOT NULL,
    object_id    TEXT        NOT NULL,
    -- The fully-marshalled message, so the relay never re-derives it (and a contract change
    -- cannot retroactively alter a pending message).
    payload      JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- published_at IS NULL means pending. Rows are kept after publish so a run can be audited;
    -- pruning is a separate operational concern.
    published_at TIMESTAMPTZ,
    attempts     INT         NOT NULL DEFAULT 0,
    last_error   TEXT,
    -- When the last delivery attempt failed. Drives exponential backoff: without it, a row that
    -- can never be delivered (a "poison" message) is re-selected on EVERY pass, and once enough
    -- of them accumulate as the oldest resource heads they consume the whole batch forever —
    -- starving every other resource rather than blocking only their own.
    last_attempt_at TIMESTAMPTZ
);

-- The relay's claim query. PARTIAL, so the index stays small — it never grows with published
-- history, which is the bulk of the table.
--
-- Keyed (object_type, object_id, id), not (created_at): the relay claims at most ONE pending row
-- per resource, gated on "no older pending row exists for the same object", and this composite
-- serves both that predecessor check and the ORDER BY id. Without it the NOT EXISTS re-scans
-- retained history on every pass.
--
-- Ordering is by id rather than created_at because created_at defaults to now(), which is
-- TRANSACTION-START time in PostgreSQL: a transaction that began earlier but wrote later gets an
-- earlier created_at, so sorting by it can invert the committed order of two mutations.
CREATE INDEX IF NOT EXISTS idx_index_outbox_pending
    ON index_outbox (object_type, object_id, id)
    WHERE published_at IS NULL;

-- NO index on published_at. The retention prune selects `ORDER BY id LIMIT n`, which the planner
-- serves from the primary key while filtering on published_at — verified with EXPLAIN on 20k
-- rows. A published_at index is never chosen for that shape, so it would be pure write
-- amplification on the hottest column in the table.
