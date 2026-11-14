# 2026-11-14 — Reddit gender, language and dayparting at create (#549)

**Update** — A Reddit create accepts `redditConfig.gender` (`"female"` | `"male"`, sent as the ad
group's `targeting.gender`), `redditConfig.languages` (ISO 639-1 codes from Reddit's enum, sent as
`targeting.languages`) and `redditConfig.schedule` (weekly windows
`{startDay, startHour, endDay, endHour}`, sent as the campaign's `schedule`, which replaces every
ad group's under campaign budget optimization). This is gap 13 of the Reddit Ads gap review; the
shapes are from Reddit's public v3 OpenAPI document. Gender and languages are sent on every
ad-group attempt, so the communities/interests retry keeps them.

`reddit.NormalizeDemographicTargeting` holds the rules. `RedditDispatcher.PreflightCreate` refuses
a wrong JSON type, an unknown or blank value, an incomplete, out-of-range, zero-length or repeated
window, more than 50 windows, or any of the three while the flag is off with a synchronous 400;
`Dispatch` and the client re-check before any Reddit request. Behind
`REDDIT_GENDER_LANGUAGE_DAYPARTING_ENABLED` (chart `"false"`) until a live create confirms Reddit
accepts them, including whether a window's end hour is inclusive. Gap review: #549.
