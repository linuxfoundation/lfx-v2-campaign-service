# 2026-11-04 — #460 Demand Gen ad-group audiences gated behind the audience flag

**Update** — #469 (#459) gated `googleAdsConfig.demandGenAudience` behind
`GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED`, because the client checks the Audience against the
Google Ads account only and the LF account is shared by every foundation (#468). #460's
`demandGenAdGroups[].audience` is the same input on a named ad group, and the list replaces
the single ad group, so a caller could have moved the gated Audience into a group.

`Dispatch` now refuses any `demandGenAdGroups` entry with a non-empty `audience` (whitespace
included) as not-created, before the budget, unless the flag is exactly `"true"`. The check
is `googleAdsDemandGenAdGroupAudiencesSet`, right after the `demandGenAudience` gate, and the
refusal goes through the shared `googleAdsInputDisabled`, so the asynchronous create job
shows the gate's sentence. The sentence names the entries by 1-based position and carries
no caller value. A group without an audience is not gated. The chart default stays
`"false"`; README, the deployment concept, the api-catalog and the googleads and dispatch
concepts say the flag now covers both inputs.

Nothing else on #460 names a resource of the shared Google Ads account: the video ad's
YouTube ids are YouTube videos, not account resources; its logo and every group's images
are fetched URLs or stored assets read under the brief's own project and brief; and the
call to action is a fixed enum.
