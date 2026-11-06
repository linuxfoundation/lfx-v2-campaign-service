# 2026-11-06 — #516 custom audiences inside the Demand Gen demographics Audience

**Update** — `googleAdsConfig.demandGenDemographics.customAudiences`
(`DemandGenDemographics.CustomAudiences`) puts EXISTING custom audiences
(`customers/<this account>/customAudiences/<id>`, at most 20, deduped) in the Audience that
#498 creates. They go in the same `audienceSegments` dimension as the user lists, after them,
as `{customAudience: {customAudience}}`. The live "CNCF - KubeCon NA - High Intent" Audience
(`audiences/358194767`) is built that way: age 25-54, three user lists and the custom-intent
segment `customAudiences/776852670` (read-only GAQL, 2026-11-06). Before this, LFX could only
reference that Audience, not build one like it for a new event.

- `customAudiences` alone is a complete Audience.
- Pre-flight refusals: a user list there (move it to `audienceSegments`), and a custom
  audience in `audienceSegments` (move it to `customAudiences`); an Audience; a legacy
  `customInterests/…` resource; a `userInterests/…`; another customer's resource; a
  non-canonical id; more than 20.
- The name hash covers the custom audiences. A user-list-only plan sends the same bytes as
  before, so its name, and the Audience a retry reuses, are unchanged. The reuse read-back
  compares custom-audience segments too.
- Gates: as `audienceSegments`, `GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED` (the refusal names
  every gated list set) and `global_marketing_ops` on create
  (`demandGenDemographics.customAudiences` in `service.GoogleAdsMarketingOpsInputs`), under
  the field's `GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED`.
- The step summary counts them ("1 custom audience(s)").

Probes on 2026-11-06, account 8666746580, v23, validateOnly, nothing persisted. The bodies
were dumped from `validateDemandGenDemographics` and sent verbatim:
- **Accepted:** age 25-54 (+ undetermined), three user lists and a custom audience; a custom
  audience alone; the first attached by temporary id to PAUSED LFX Demand Gen ad group
  202741618278 in one `googleAds:mutate`.
- **Refused:** a legacy `customInterests/2808384` as an Audience segment
  (`RESOURCE_NAME_MALFORMED`, expected `userInterests`), which is why it is refused before any
  request.

Still not verified: a persisted create and its GAQL read-back (the #498 caveat).
