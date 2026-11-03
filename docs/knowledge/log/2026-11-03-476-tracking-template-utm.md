# 2026-11-03 — #476 Google campaigns fill utm_campaign; utm_medium per channel

**Update** — The Google Ads account's tracking template sets `utm_campaign={_utmcampaign}`,
which reads a campaign custom parameter named `utmcampaign`. Hand-built campaigns override
the template with a literal campaign name; LFX campaigns set no parameter, so the template's
`utm_campaign` came out empty on their clicks (the final URL's event-level `utm_campaign` was
unaffected). LFX also put `utm_medium=cpc` on every final URL, while the
team uses `paid-search` on Search and `display` on Demand Gen, Display and YouTube.

Every channel's campaign create now sends
`urlCustomParameters: [{key: "utmcampaign", value: url.QueryEscape(<composed name>)}]`
(`internal/platform/googleads/tracking.go`, wired through `preflightCampaignKind` into the five
create payloads). The escape matches the hand-built campaigns' encoding: space `+`, `|` `%7C`,
`+` `%2B`. An escaped value over 250 characters is left out. The length decision is made in
the preflight, before the budget mutate, so it can never be what fails a create after its
budget exists, and the cascade records `tracking template's utm_campaign left empty: …` after
its `Campaign created` step. Adoption sends nothing.

`utmMediumFor` gives the final URLs' `utm_medium`: `paid-search` for Search, `display` for
Demand Gen, Display and Video, and `cpc` kept for Performance Max, which spans both. Search's
sitelink, promotion and price destinations take Search's medium. `utm_campaign` and
`utm_content` on the URLs are unchanged, and `utm_source`/`utm_medium` still replace any value
the caller's URL carried, as before.

The settings readback does not read `url_custom_parameters`: the value is derived from the
name the readback already reports, and no row records it.

Verified with validateOnly only, on 2026-11-03 (v23): a `url.QueryEscape`'d campaign name was
accepted on Search, Demand Gen, Display and Performance Max campaign creates; a raw name was
refused (`ILLEGAL_CHARS`) on Search, Demand Gen and Display; `_utmcampaign` was refused; 251
characters was `TOO_LONG`. Video was not probed — its creation is refused before any request.

lf-dbt: the account has auto-tagging on, so every Google click carries a click id, and
`macros/classify_web_session_channel.sql` classifies a gclid/gbraid/wbraid session as Paid
Search whatever its `utm_medium`; `paid-search` also matches its `%paid%search%` rule. A Demand
Gen, Display or Video session that has lost its click id no longer matches the utm-only Paid
Search rule (`cpc` did) and falls to a later branch — the same way the team's own Demand Gen
and YouTube campaigns, which already use `display`, are classified.
`_silver_fact_ad_session_attribution.sql` accepts all three mediums (`%paid%`, or `cpc`, `ppc`,
`display`, `video`).

Tests: `internal/platform/googleads/tracking_test.go` (the live encoding, escaping of
`&`, `=`, `#` and `{}`, the 250/251 boundary, the parameter and `utm_medium` on all five
channels including Search's extension URLs, the omission step on all five and on a later
failure's partial result) and
`TestGoogleAds_Adoption_SendsNoUTMCampaignParameter`.
