# 2026-11-03 — #476 Google campaigns fill utm_campaign; utm_medium per channel

**Update** — The Google Ads account's tracking template sets `utm_campaign={_utmcampaign}`,
which reads a campaign custom parameter named `utmcampaign`. Hand-built campaigns override
the template with a literal campaign name; LFX campaigns set no parameter, so their clicks
arrived with `utm_campaign` empty. LFX also put `utm_medium=cpc` on every final URL, while the
team uses `paid-search` on Search and `display` on Demand Gen, Display and YouTube.

Every channel's campaign create now sends
`urlCustomParameters: [{key: "utmcampaign", value: url.QueryEscape(<composed name>)}]`
(`internal/platform/googleads/tracking.go`, wired through `preflightCampaignKind` into the five
create payloads). The escape matches the hand-built campaigns' encoding: space `+`, `|` `%7C`,
`+` `%2B`. An escaped value over 250 characters is left out, decided in the preflight so it
can never fail a create after the budget exists, and the cascade records
`utm_campaign left empty: …` after its `Campaign created` step. Adoption sends nothing.

`utmMediumFor` gives the final URLs' `utm_medium`: `paid-search` for Search, `display` for
Demand Gen, Display and Video, and `cpc` kept for Performance Max, which spans both. Search's
sitelink, promotion and price destinations take Search's medium. `utm_campaign` and
`utm_content` on the URLs are unchanged, and `utm_source`/`utm_medium` still replace any value
the caller's URL carried, as before.

The settings readback does not read `url_custom_parameters`: the value is derived from the
name the readback already reports, and no row records it.

Verified with validateOnly only, on 2026-11-03 (v23): the escaped name was accepted on Search;
a raw name was refused (`ILLEGAL_CHARS`) on Search, Demand Gen and Display; `_utmcampaign` was
refused; 251 characters was `TOO_LONG`. Performance Max and Video were not probed.

lf-dbt is unaffected: `macros/classify_web_session_channel.sql` classifies any `gclid` click
as Paid Search, and `_silver_fact_ad_session_attribution.sql` accepts `utm_medium` in
(`cpc`, `ppc`, `display`, `video`) or `LIKE '%paid%'`, which `paid-search` matches.

Tests: `internal/platform/googleads/tracking_test.go` (the live encoding, the 250/251
boundary, the parameter and `utm_medium` on all five channels including Search's extension
URLs, the omission step on all five and on a later failure's partial result) and
`TestGoogleAds_Adoption_SendsNoUTMCampaignParameter`.
