# 2026-11-05 — Demand Gen audiences require global_marketing_ops

**Update** — `googleAdsConfig.demandGenAudience` (#459) and each
`demandGenAdGroups[].audience` (#460) name an existing Audience on the Google Ads account every
foundation shares. Until now the only check on them was the per-deployment flag
`GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED`, on the reasoning that `campaign_manager` is the LF
marketing team. It is not: lfx-v2-helm's `model.fga` defines it as
`executive_director or global_marketing_ops`, so turning the flag on would let one foundation's
executive director target audiences built for another.

`BriefService.CreateCampaigns` now requires `global_marketing_ops` on the path project for any
create that selects `google-ads` and sets either input (any non-empty value, whitespace
included). The gateway cannot do it, since the input is in the body, so the service asks over
the NATS connection the indexer already uses: `lfx.projects-api.slug_to_uid` (project-service)
for the UID, then `lfx.access_check.request` (fga-sync) with
`project:<uid>#global_marketing_ops@user:<username>`. Only an exact `true` line for that token
admits the request; `false` is `403`, and any other reply, a timeout, an empty `NATS_URL` or a
missing username is `503`. Nothing is created in either case.

`service.GoogleAdsDemandGenAudienceRequested` detects the inputs;
`TestDemandGenAudienceDetectionAgreesWithTheCreateGate` (`internal/dispatch`) pins it to the
dispatcher's own detection, so a config the dispatcher attaches an Audience for cannot pass the
create path unchecked. Removing the call, dropping the group-audience loop, admitting on a check
error, matching a reply line by verdict alone, and sending a username with a separator or
newline each fail the tests.

The flag is unchanged and still off; it can be turned on once this ships. The Display,
Performance Max and shared-negative-list flags have no such check yet and should stay off.
