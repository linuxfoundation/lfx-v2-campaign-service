-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- LinkedIn account grants (#581): the operator-installed allow-list of extra (ad account,
-- organization) pairs the LF SYSTEM LinkedIn connection may address besides its own account_id /
-- org_id. One LinkedIn token reaches several ad accounts (LF Events, CNCF, TLF, ...), and LF runs
-- each one under its own organization page, so a system row bound to exactly one account could
-- neither create in nor pause campaigns on the others.
--
-- TYPED, NOT A JSON BLOB (D3): each grant is a row, so the database itself refuses a malformed
-- id or a second grant for the same account on one connection, and the org is stored beside its
-- account rather than inferred from anything the caller sends. The list, not every account the
-- token can reach, is the boundary: an account absent here is never addressed through the system
-- row, however reachable it is.
--
-- WRITER: only `campaign-service bootstrap-system-account -provider linkedin-ads
-- -linkedin-grant ...`. The public connection API never reads or writes this table, and the
-- installer refuses a grant on anything but the system scope. The replace runs in one transaction
-- that also bumps the parent row's version, so a cached credential/client for the old grant set is
-- invalidated by the same rotation rule a credential change uses.
--
-- ON DELETE CASCADE covers a HARD delete of the connection row only. A soft-deleted row
-- (status = 'deleted') keeps its grants, and nothing reads them: the dispatcher loads grants by
-- the LIVE row's id, so a tombstone's grants are inert, and a re-installed system row starts with
-- none until the installer writes them again.
--
-- ADD ONLY: a new table no N-1 binary reads, so a rolling deploy cannot break on it.
CREATE TABLE IF NOT EXISTS linkedin_ads_account_grants (
    connection_id UUID        NOT NULL REFERENCES linkedin_ads_connections (id) ON DELETE CASCADE,
    account_id    TEXT        NOT NULL CHECK (account_id ~ '^[0-9]+$'),
    org_id        TEXT        NOT NULL CHECK (org_id ~ '^[0-9]+$'),
    label         TEXT        CHECK (label IS NULL OR char_length(label) <= 200),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, account_id)
);
