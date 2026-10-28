-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore the two-surface CHECKs from 000030.
--
-- NARROWING, so it is only safe once no `survey` row exists: the ADD CONSTRAINT below is validated
-- against the whole table and FAILS if one does. This file is multi-statement and so runs in one
-- implicit transaction; that failure aborts it, rolling back the DROPs too, so the table keeps
-- 000052's CHECKs (golang-migrate records the version dirty until it is cleared). That is the
-- honest outcome -- a down migration must not silently delete briefs to make itself apply.
--
-- What it means for an operator rolling back a release that shipped survey briefs: those rows,
-- and their campaigns and audiences, must first be DELETED or migrated to a two-surface identity
-- by SQL. Archiving does not help -- it changes only `status`, and the CHECK reads
-- `delivery_type` and `stage` -- and the API cannot do it either, because a brief's delivery type
-- and stage are identity that replace refuses to change.
--
-- The DROPs run before the ADDs so a partially-applied up migration still rolls back cleanly.

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_stage_pair_valid;

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_type_valid
    CHECK (delivery_type IN ('paid-marketing', 'email'));

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_stage_pair_valid
    CHECK (
        (delivery_type = 'paid-marketing' AND stage = '')
        OR (delivery_type = 'email' AND stage IN (
            'CFP Launch', 'Schedule Announcement', 'Registration Push',
            'Discount Offer', 'Final Countdown', 'Post-Event'
        ))
    );
