-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping this DESTROYS provenance that cannot be recovered: the portal a row's list ids were
-- verified in is derivable from nothing else, because the ids are bare numerics and the credential
-- that created them may since have been rotated or repointed. Re-applying 000032 restores the
-- COLUMN, not the values, so every audience built before the rollback comes back unstamped.
--
-- The consequence is not a lost audit trail but a stopped service: the dispatch guard refuses an
-- audience recording no portal (ErrCampaignProvenanceUnknown), so after a down/up cycle every
-- affected email send fails closed until its audience is rebuilt. That is the correct refusal --
-- an unstamped row genuinely cannot be proven -- which is exactly why the values are worth more
-- than the column.
--
-- Down is provided for migration symmetry, not as a routine operation. If a rollback is
-- unavoidable, plan the rebuild of every `built` HubSpot audience as part of it.

ALTER TABLE campaign_audiences DROP COLUMN IF EXISTS built_in_portal_id;
