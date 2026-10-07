-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting drops include_list_ids. platform_master_list_id holds each row's FIRST include id, so
-- a row attached to ONE list loses nothing. A row attached to SEVERAL lists would lose every list
-- but the first while staying built, and the previous dispatcher would then stage a send and report
-- success while omitting those recipients. That is a silent audience change, so the revert REFUSES
-- while any such row can still be sent (the same guard 000030's down uses for a lossy drop).
--
-- "Can still be sent" is a BUILT audience on a brief that is not archived: only those are
-- dispatched. An archived brief's audience keeps its history and does not block the revert, so
-- archiving the brief IS a recovery path, as is re-attaching the audience to a single master list.
-- In one transaction, so a refusal leaves the column and its CHECK in place.

BEGIN;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM campaign_audiences a
        JOIN campaign_briefs b ON b.id = a.brief_id
        WHERE a.status = 'built'
          AND b.status <> 'archived'
          AND a.include_list_ids IS NOT NULL
          AND jsonb_typeof(a.include_list_ids) = 'array'
          AND jsonb_array_length(a.include_list_ids) > 1
    ) THEN
        RAISE EXCEPTION
            'cannot revert 000040: built audiences on live briefs are attached to several include lists. '
            'Dropping include_list_ids would narrow each to its first list while it stays built. '
            'Re-attach those audiences to a single master list, or archive their briefs, first.';
    END IF;
END $$;

ALTER TABLE campaign_audiences DROP COLUMN IF EXISTS include_list_ids;

COMMIT;
