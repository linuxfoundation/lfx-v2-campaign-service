-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Enforce the built-audience invariant at the datastore, not only in the API.
-- AudienceBuilt is DEFINED as "the platform master list exists", so a row with
-- status='built' MUST carry a platform_master_list_id. The service layer already
-- rejects this with a 400 on create/update, but that protects only the two API
-- handlers — the platform build worker and any direct write / backfill could still
-- flip status to 'built' while leaving the pointer NULL, producing a "built"
-- audience that answers "what did we send to?" with nothing. This CHECK makes the
-- database the source of truth; the API 400 is then a friendly early reject.
--
-- The repo writes an empty platform_master_list_id as SQL NULL (nullStr), so via the
-- API the NULL test already covers the omitted + explicitly-cleared cases. But the DB
-- is meant to be the source of truth for ALL writers (the build worker, backfills,
-- direct writes) — and those could persist an empty or whitespace-only string that is
-- NOT NULL. So require a genuinely non-blank pointer: reject '' and whitespace by
-- trimming before the emptiness test.
ALTER TABLE campaign_audiences
    ADD CONSTRAINT campaign_audiences_built_needs_master_list
    CHECK (
        status <> 'built'
        OR (platform_master_list_id IS NOT NULL AND btrim(platform_master_list_id) <> '')
    );
