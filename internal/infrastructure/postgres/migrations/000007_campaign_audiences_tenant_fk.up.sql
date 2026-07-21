-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Enforce the parent-tenant relationship at the datastore: a campaign_audiences row's
-- project_id MUST match the project_id of its parent brief. The existing FK on
-- migration 000005 references only campaign_briefs(id), so brief_id is valid but the
-- copied project_id is unchecked — a worker/backfill/direct write could persist an
-- audience whose project_id belongs to a DIFFERENT project than its brief, and
-- GetAudience (which trusts the stored project_id for tenant scoping) would then expose
-- it under the wrong tenant. The API create path already guards this (INSERT … WHERE
-- EXISTS an active brief scoped by project+brief), but the DB is meant to be the source
-- of truth for ALL writers — so make the pair a composite foreign key.
--
-- A composite FK needs a UNIQUE (or PK) on the referenced columns. campaign_briefs.id is
-- already the PK (so id is unique), but a composite FK must reference a uniquely-
-- constrained COLUMN SET; add UNIQUE (id, project_id). It is redundant with the PK for
-- uniqueness (id alone is unique) but is what lets (brief_id, project_id) reference it.
ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_id_project_id_key UNIQUE (id, project_id);

-- Swap the brief_id-only FK for the composite (brief_id, project_id) FK.
ALTER TABLE campaign_audiences
    DROP CONSTRAINT IF EXISTS campaign_audiences_brief_id_fkey;

ALTER TABLE campaign_audiences
    ADD CONSTRAINT campaign_audiences_brief_project_fkey
    FOREIGN KEY (brief_id, project_id)
    REFERENCES campaign_briefs (id, project_id);
