# 2026-11-14 — #550 Meta region and city targeting (gap 13)

**Creation** — Gap 13 of the Meta gap review (#550). A Meta ad set could target whole countries
only. `metaConfig.targeting.regions` and `targeting.cities` (numeric Meta geo-location keys, at most
25 each) are added to the country targeting as `geo_locations.regions` / `.cities`. The owner decided
(2026-11-14) that every key is looked up before create and its parent country must pass the existing
sanctioned-country check.

One `GET /search?type=adgeolocationmeta` read answers every key after the account preflight and
before any write. A parent country that is sanctioned or ineligible, a regulated market, already a
country target (Meta refuses overlapping locations) or an EU territory without the DSA disclosures
is a refusal; a key of the other type is too. A failed read, an unanswered key or one without a
country is an error. Either way nothing is created. With no geoTargets the US default is dropped, so
a region-only create targets just that region and is named after its country.

Behind its own default-off flag, `META_REGION_TARGETING_ENABLED`, with the locale flag's rules. Not
gated by `global_marketing_ops`. Left for later: a city radius and zip codes.

Tests: `geo_targeting_test.go` (the additive payload, a region-only create, twelve unusable answers
with no POST, an EU city with disclosures, normalization) and `meta_region_targeting_test.go` (flag
off and on, the flag interplay, a wrong type, Dispatch's wiring and refusal with no request).
Seventeen hand mutations each fail a test.
