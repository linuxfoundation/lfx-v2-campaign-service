# 2026-11-25 — #664 LinkedIn Lead Gen Form campaigns

**Update** — `linkedInConfig.objective` takes `"lead_generation"` (#552 gap 6) with
`leadGenForm`, a Lead Gen Form built in Campaign Manager (its id or `urn:li:adForm:<id>`), and an
optional `leadGenCallToAction` button label (default `REGISTER`), behind
`LINKEDIN_ADS_LEAD_GEN_ENABLED` (off by default). The campaign is `LEAD_GENERATION` / `MAX_LEAD`
on CPM, automatic bidding only, name segment `Leads`; every creative carries
`leadgenCallToAction` naming the form. With the flag off, a create naming any of the three is a
400 before a job exists. So is a missing or malformed form, a form without the objective, a manual
bid, an unknown label, a text create, the Audience Network, and a variant call to action. Dispatch
repeats the checks, and the client reads the form before the campaign group exists: another
owner's form, an archived one, or a 404 creates nothing. `leadGenForm` requires
`global_marketing_ops`.

`GET /projects/{project_id}/linkedin-ads/lead-forms` lists the account's non-archived forms for
the picker, gated on `global_marketing_ops` (marketing-ops-api) and answering 400 while the flag is
off. The defaults are unchanged: without the objective, the create sends the objective, bidding
and `offsiteDeliveryEnabled: false` it always has. Not yet created on a live account; the paused
probe is recorded in the PR.
