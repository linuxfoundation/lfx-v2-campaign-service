-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting drops include_list_ids. platform_master_list_id holds each row's FIRST include id, so
-- a row attached to ONE list loses nothing. A row attached to SEVERAL lists would lose every list
-- but the first while staying sendable, and the previous dispatcher would then stage a send and
-- report success while omitting those recipients. That is a silent audience change, so the revert
-- REFUSES while such a row is in a position to be sent (the same guard 000030's down uses for a
-- lossy drop).
--
-- "In a position to be sent" follows how dispatch picks a brief's audience -- the NEWEST BUILT row
-- for the platform (internal/dispatch/hubspot.go) -- on briefs that are not archived. The guard
-- checks, per live brief and platform, the newest built row (what dispatch reads now) AND the
-- newest row of any status (a failed or building attach that can be patched to built). Older rows
-- that a newer attach superseded are history and do not block.
--
-- Recovery, either way: re-attach the audience to a single master list (that new row becomes the
-- newest, and the newest built once it is built), or archive the brief. In one transaction, so a
-- refusal leaves the column and its CHECK in place.

BEGIN;

DO $$
BEGIN
    IF EXISTS (
        WITH live AS (
            SELECT a.id, a.brief_id, a.platform, a.status, a.created_at, a.include_list_ids
            FROM campaign_audiences a
            JOIN campaign_briefs b ON b.id = a.brief_id
            WHERE b.status <> 'archived'
        ),
        newest AS (
            SELECT DISTINCT ON (brief_id, platform) include_list_ids
            FROM live
            ORDER BY brief_id, platform, created_at DESC, id DESC
        ),
        newest_built AS (
            SELECT DISTINCT ON (brief_id, platform) include_list_ids
            FROM live
            WHERE status = 'built'
            ORDER BY brief_id, platform, created_at DESC, id DESC
        )
        SELECT 1 FROM (SELECT include_list_ids FROM newest UNION ALL SELECT include_list_ids FROM newest_built) candidate
        WHERE include_list_ids IS NOT NULL
          AND jsonb_typeof(include_list_ids) = 'array'
          AND jsonb_array_length(include_list_ids) > 1
    ) THEN
        RAISE EXCEPTION
            'cannot revert 000040: a live brief''s current audience is attached to several include lists. '
            'Dropping include_list_ids would narrow it to its first list. '
            'Re-attach that audience to a single master list, or archive the brief, first.';
    END IF;
END $$;

ALTER TABLE campaign_audiences DROP COLUMN IF EXISTS include_list_ids;

COMMIT;
