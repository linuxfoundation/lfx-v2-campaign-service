-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Lease columns for the outbox relay.
--
-- The relay used to hold ONE transaction open across the whole pass: claim, publish, retire.
-- FOR UPDATE SKIP LOCKED made exclusivity free, because the row locks lasted the whole pass.
-- But publishing is a NATS request/reply, so a pool connection stayed checked out for up to
-- relayPassTimeout (30s) while the relay waited on the broker. With a small pool that blocks
-- every brief/campaign write and the readiness query — the broker degradation this table exists
-- to isolate turns into a service outage — and pool.Close() at shutdown waits on that connection,
-- defeating the bounded Stop.
--
-- Publishing now happens OUTSIDE any transaction, which means row locks can no longer carry
-- exclusivity across the publish. These columns carry it instead:
--
--   leased_until  when the claim expires. A crashed pod's rows become claimable again after
--                 this passes, rather than wedging their resource forever. Compared against
--                 clock_timestamp(), never now(): now() is TRANSACTION-START time, so under
--                 any transaction it understates elapsed time and would hand the same row to
--                 two pods.
--   leased_by     which pod holds it. Not required for correctness — the retire is guarded on
--                 the lease being unexpired — but without it an operator staring at a stuck
--                 resource cannot tell WHICH replica is wedged.
ALTER TABLE index_outbox
    ADD COLUMN IF NOT EXISTS leased_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS leased_by    TEXT;

-- The claim's partial index, replacing idx_index_outbox_pending.
--
-- The claim now filters on (published_at IS NULL AND lease is free) and probes
-- (object_type, object_id, id) for both the predecessor check and the ORDER BY. leased_until is
-- INCLUDED rather than keyed: keying it would order rows by lease expiry, which is not the scan
-- order the predecessor check needs, and every lease write would move the row within the index.
-- As a payload column it still lets the visibility check run index-only.
DROP INDEX IF EXISTS idx_index_outbox_pending;

CREATE INDEX IF NOT EXISTS idx_index_outbox_claimable
    ON index_outbox (object_type, object_id, id)
    INCLUDE (leased_until)
    WHERE published_at IS NULL;
