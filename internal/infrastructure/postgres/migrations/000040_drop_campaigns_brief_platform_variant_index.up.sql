-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- CONTRACT step of the slot-version change: drop 000022's three-column slot index,
-- uq_campaigns_brief_platform_variant_live, now that 000037's four-column
-- uq_campaigns_brief_platform_variant_slot_version_live has been the ON CONFLICT arbiter of
-- every claim, upsert and adopt for a full release.
--
-- Safe for the N-1 binary (the one that shipped 000037): none of its statements names the
-- three-column index as an arbiter. It only CLASSIFIED a 23505 raised on this index, as
-- ErrSlotVersionUnavailable above slot 1 and as a lost race at slot 1, and once the index is
-- gone that 23505 simply never happens — a slot-1 race resolves on the arbiter as DO NOTHING,
-- and a slot-2 claim succeeds, which is what new_version asked for.
--
-- What the index used to provide for free — "an adopt binds only an EMPTY slot" — is
-- re-established in code, not here: ClaimCampaignDispatch, UpsertCampaign and AdoptCampaign
-- now take a transaction-scoped advisory lock per (brief, platform, variant), and the adopt
-- checks for ANY live row on the slot under that lock before it inserts. The four-column
-- index stays the arbiter for same-slot-version races. See campaign_repo.go.
--
-- Bare and alone, as 000024 is: DROP INDEX CONCURRENTLY cannot run inside a transaction, and
-- a multi-statement file is batched into one. CONCURRENTLY because migrations run during a
-- rolling restart while other replicas are claiming dispatches, and a blocking drop would
-- stall a claim mid-flight. No verify step precedes it (unlike 000023 before 000024): the
-- index that takes over is already the arbiter every statement names, and it is in
-- requiredIndexes, so boot and /readyz fail closed if it is missing or invalid.

DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_brief_platform_variant_live;
