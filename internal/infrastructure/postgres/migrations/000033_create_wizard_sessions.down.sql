-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the table DISCARDS every in-flight and completed wizard run: the plan, both
-- generated content variants, the edited sections and the full chat history exist nowhere
-- else in this service. What SURVIVES is the part that left this service -- the HubSpot
-- draft a completed session created, and the campaign/audience rows the clone and
-- set-send-list turns wrote through the ordinary model -- so a re-migration loses the
-- wizard's working state and its conversation, not the artifact it produced. Any session
-- mid-run at the time is unrecoverable and the caller must start the wizard again.
DROP TABLE IF EXISTS wizard_sessions;
