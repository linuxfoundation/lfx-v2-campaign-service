-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Admit `survey` as a third delivery surface, alongside paid-marketing and email.
--
-- 000052: first numbered 000047 on its branch; main claimed 000047-000051 while it was open, so
-- it moved above them. golang-migrate never applies a version lower than the highest applied one,
-- so a branch migration must always sit above main's.
--
-- 000030 enumerated the two surfaces that existed then and paired each with the stages it may
-- carry. Survey/report promotion is a THIRD surface, not a seventh email stage: its brief is
-- authored from a free-text source document and a promoted URL rather than from a scraped event
-- page, and its sends are not points on an event's lifecycle. Modelling it as an email stage
-- would put a survey brief in the same identity slot as that event's sends and let one displace
-- the other, which is the defect 000030 exists to prevent.
--
-- WIDENING ONLY, so this is expand-safe in a single release (see this directory's README): every
-- row that satisfied the old CHECKs still satisfies these, and an N-1 binary that never writes
-- `survey` is unaffected. Nothing is dropped or narrowed, so no contract step follows.
--
-- NOT `CONCURRENTLY`: this file is multi-statement and therefore runs in an implicit transaction,
-- where CONCURRENTLY is forbidden. It also adds no index.

-- Reject a delivery type the service cannot act on. Enumerated rather than free text because the
-- value selects which generators run and which surface may open the row; an unrecognised one
-- would be silently treated as paid by every reader.
ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_type_valid
    CHECK (delivery_type IN ('paid-marketing', 'email', 'survey'));

-- Reject the identity COMBINATIONS the model says cannot exist, not merely the three values
-- independently -- see 000030 for why the pair is enforced here rather than only at the HTTP edge.
--
-- A survey brief's stage names WHICH ASSET is being promoted (a survey instrument or a published
-- report), where an email brief's stage names WHICH SEND in a series it is. Both are the brief's
-- identity, so both belong in the key; they are simply different vocabularies on different
-- surfaces, which is why this CHECK pairs each surface with its own list rather than pooling them.
-- Pooling would admit `(email, 'Survey Promo')` -- a slot no generator can serve.
--
-- Keep the email list in step with `emailstage.Names()` and the survey list with
-- `emailstage.SurveyNames()`. `TestLivePairCheckMatchesEmailStageNames` and
-- `TestLivePairCheckMatchesSurveyStageNames` pin both; a new stage on either surface needs a
-- migration to reach this constraint.
ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_stage_pair_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_stage_pair_valid
    CHECK (
        (delivery_type = 'paid-marketing' AND stage = '')
        OR (delivery_type = 'email' AND stage IN (
            'CFP Launch', 'Schedule Announcement', 'Registration Push',
            'Discount Offer', 'Final Countdown', 'Post-Event'
        ))
        OR (delivery_type = 'survey' AND stage IN (
            'Survey Promo', 'Report Promo'
        ))
    );
