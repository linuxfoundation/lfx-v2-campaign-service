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
    last_error   TEXT
);

-- The relay's only query: oldest pending first. PARTIAL, so the index stays small — it never
-- grows with published history, which is the bulk of the table.
CREATE INDEX IF NOT EXISTS idx_index_outbox_pending
    ON index_outbox (created_at)
    WHERE published_at IS NULL;
