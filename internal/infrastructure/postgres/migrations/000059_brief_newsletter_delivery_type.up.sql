-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Admit the NEWSLETTER surface: one issue of a brand's recurring HubSpot newsletter.
--
-- A newsletter brief is authored from the operator's notes and drafted from the brand's previous
-- published newsletter, so it is neither an event send nor a survey promotion. It gets its own
-- delivery type and its own single stage, `Newsletter`; issues are told apart by event_slug.
--
-- WIDENING ONLY, so this is expand-safe in a single release (see this directory's README): every
-- row that satisfied 000052/000053 still satisfies this, and an N-1 binary that never writes the
-- new surface is unaffected.
--
-- Keep the newsletter arm in step with `emailstage.NewsletterNames()`;
-- `TestLivePairCheckMatchesNewsletterStageNames` pins it.
ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_type_valid
    CHECK (delivery_type IN ('paid-marketing', 'email', 'survey', 'newsletter'));

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
            'Survey Promo', 'Survey Reminder', 'Survey Closing', 'Report Promo'
        ))
        OR (delivery_type = 'newsletter' AND stage IN (
            'Newsletter'
        ))
    );
