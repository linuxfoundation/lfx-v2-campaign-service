-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Admit the survey surface's REMINDER and CLOSING stages.
--
-- 000053: first numbered 000048 on its branch, renumbered with 000052 for the same reason.
--
-- 000052 gave the survey surface two stages, one per asset: `Survey Promo` and `Report Promo`.
-- That modelled WHICH ASSET is promoted but not WHICH SEND in a series, so a survey campaign that
-- goes out as an invite and then a reminder had nowhere to put the second email: both sends would
-- have keyed to `Survey Promo` and the later one would have displaced the first.
--
-- A survey's promotion is time-bounded -- the instrument closes -- so the series is the same shape
-- the email surface already has: an opening send, a "still open" reminder, and a closing push. The
-- three are separate briefs for the reason every stage is: each is generated, audienced and staged
-- on its own, and each becomes its own HubSpot draft.
--
-- Report promotion is deliberately NOT given the same pair. A published report does not close, so
-- a "closing" send names a deadline that cannot exist, and the anti-invention rule the survey
-- prompts carry would have nothing to ground it in. `Report Promo` stays a single send until there
-- is a real campaign shape asking otherwise.
--
-- WIDENING ONLY, so this is expand-safe in a single release (see this directory's README): every
-- row that satisfied 000052 still satisfies this, and an N-1 binary that never writes the new
-- stages is unaffected. Nothing is dropped or narrowed, so no contract step follows.
--
-- Only the PAIR check changes. The delivery-type CHECK from 000052 already admits `survey` and is
-- untouched here, so it is deliberately not re-stated: re-dropping and re-adding a constraint this
-- migration does not alter would make a reader look for a change that is not there.

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
            'Survey Promo', 'Survey Reminder', 'Survey Closing', 'Report Promo'
        ))
    );
