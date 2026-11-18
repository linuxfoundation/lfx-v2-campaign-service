# 2026-11-18 — #549 Reddit sub-country geolocation targeting (gap 11)

**Update** — A Reddit ad group could target whole countries only (ISO alpha-2). The new
`redditConfig.subCountryGeoTargets` field takes Reddit region, city and DMA geolocation ids
(`US-CA`, `CA:6167865`, `662`). They are sent in `targeting.geolocations` after the countries.

- `reddit.NormalizeSubCountryGeoTargets` holds the rules. It refuses a malformed id, a bare
  country, more than 50 ids, and an id inside a country `geoTargets` targets or
  `excludedGeoTargets` excludes. With no countries the `US` default is dropped, and the campaign is
  named after the first id's country. The ad group name lists the countries, then the first id
  and a count of the rest, so a full list fits the name limit. The exclusion rule now reads the
  same effective targeting.
- `reddit.ValidateSubCountryGeoTargets` confirms every id with one
  `POST /targeting/geolocations_validations` before anything is created. `PreflightCreate` refuses
  an id Reddit rejects with a synchronous 400 and fails open when the read cannot run. `Dispatch`
  repeats the read and fails closed.
- A default-off deployment flag, `REDDIT_SUBCOUNTRY_GEO_ENABLED`; the chart sets `"false"`. Only
  the exact value `"true"` turns it on. Not role-gated.

Tests: `subcountry_geo_test.go` and `reddit_subcountry_geo_test.go`. Thirty-one hand mutations
each fail a test. Not verified live: no Reddit call was made, and the live probe is still owed.
