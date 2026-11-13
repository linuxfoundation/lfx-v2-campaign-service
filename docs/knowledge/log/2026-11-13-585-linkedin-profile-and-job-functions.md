# 2026-11-13 — #585 LinkedIn profile targeting ANDs job functions, and takes them from the request

**Fix** — The profile path's `targetingCriteria` was `locations AND (skills OR groups OR
jobFunctions)`, so a campaign reached every member in job functions 8, 13 and 16 in its
locations, whatever their skills or member groups. It is now `locations AND (skills OR groups)
AND jobFunctions`, the structure of LF's own Campaign Manager campaigns (lfx-self-serve#5543
ledger rows 9–10). `linkedInConfig.jobFunctions` replaces the default three; it is validated
like the targeting facet, refused as an empty list, a wrong JSON type, or together with
`targeting`, and is neither role- nor flag-gated. Every live create through campaign-service so
far stopped before its creative (#579), so no serving campaign's audience changes; campaigns
already created keep the targeting they were created with.
