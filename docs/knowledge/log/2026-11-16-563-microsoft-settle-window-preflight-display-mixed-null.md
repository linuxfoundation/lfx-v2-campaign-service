# 2026-11-16 — #563 Microsoft mixed null criterion errors

**Fix** — A #613 review finding. `postCampaignCriterions` treats an entry with both codes explicitly null as UNCONFIRMED even beside a coded rejection, and returns the ids that were created so they are recorded.
