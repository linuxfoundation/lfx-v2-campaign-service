-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Add campaigns.slot_version: the internal sequence number distinguishing a DELIBERATE
-- second campaign on one (brief, platform, variant) slot from a RETRY of the first.
--
-- Why a NEW column and not `version`. campaigns.version already exists (000002) and is the
-- OPTIMISTIC-CONCURRENCY counter: every upsert and replace bumps it, and the ETag mirrors
-- it. Keying the slot on it would give one campaign a new slot identity on every write.
-- slot_version is assigned once, at claim time, and never changes afterwards.
--
-- Why not reuse variant. `variant` names WHAT the campaign is — Google's Search vs Demand
-- Gen channel — and only Google sub-divides (model.VariantDefault). A second Search
-- campaign on the same brief is not a different variant; it is the same variant again.
-- Overloading variant to carry a counter would make it mean two things and break
-- AdoptableVariants, which enumerates the slots an adopt can bind into.
--
-- Why this is needed at all. uq_campaigns_brief_platform_variant_live keys the slot on
-- (brief_id, platform, variant), and ClaimCampaignDispatch inserts through
-- ON CONFLICT ... DO NOTHING. A second dispatch therefore LOSES the claim and
-- isReusableCampaign hands back the first campaign as a success — the operator asks for a
-- second campaign and silently receives the first.
--
-- DEFAULT 1 and NOT NULL: every existing row is the first campaign of its slot, which is
-- true by construction — the old index permitted exactly one live row per slot. The
-- default also keeps the N-1 binary writing valid rows during a rolling deploy: it does
-- not know the column, omits it, and Postgres fills 1, which is what that binary's
-- one-campaign-per-slot behaviour implies.
--
-- EXPAND-only. The new slot index arrives in 000036 (CONCURRENTLY, which must stand alone
-- in its own file), and the old index is dropped a release later per the expand/contract
-- rule in README.md.

ALTER TABLE campaigns
    ADD COLUMN IF NOT EXISTS slot_version INTEGER NOT NULL DEFAULT 1;

-- A slot version is a counting number. The CHECK is cheap insurance against a caller that
-- computes 0 from an empty read: slot_version 0 would sort below every real row and
-- occupy a slot no lookup expects.
ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_slot_version_positive;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_slot_version_positive CHECK (slot_version >= 1);
