-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drops the LinkedIn account grants (#581). Every campaign created in a granted account stays on
-- LinkedIn, and its row still records that account, but without the grant the system row no
-- longer resolves it: pause, budget, bid and metrics on such a campaign answer the account
-- mismatch refusal until the grant is re-installed. Run this only once no live campaign was
-- created in a granted account, or after re-pointing those campaigns' owners.
DROP TABLE IF EXISTS linkedin_ads_account_grants;
