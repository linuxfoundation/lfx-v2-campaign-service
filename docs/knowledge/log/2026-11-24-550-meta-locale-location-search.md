# 2026-11-24 — #550 Meta targeting search: locale and location kinds

**Update** — Create already takes `metaConfig.targeting.locales` (behind
`META_LOCALE_TARGETING_ENABLED`) and `regions` / `cities` (behind
`META_REGION_TARGETING_ENABLED`), but nothing let a picker find their keys by name. The targeting
search (`search-meta-targeting-options`) now serves two more kinds:

- `locale` → `GET /search?type=adlocale&q=&limit=50`, the keys `locales` takes. The key comes back
  as a decimal string in `id`; `locales` takes it as an integer.
- `location` → `GET /search?type=adgeolocation&q=&location_types=["region","city"]&limit=50`, the
  keys `regions` and `cities` take. Countries, zips and other location types are dropped: countries
  go in `geoTargets` as ISO codes, and create has no zip field. Each option carries
  `location_type` (`region` or `city`), `country_code` and `region` (a city's region name), all
  optional and absent for the other kinds.

Meta answers both types with `key`, not `id`, so the parser reads `key` for them and keeps a row only
when the key is what create takes (a positive whole number with no leading zero, and for a locale one
that fits create's integer). Locations are de-duplicated by type and key, since regions and cities are
separate key spaces in create. Everything else
is unchanged: the same route, `META_TARGETING_SEARCH_ENABLED` flag, `campaign_manager` gateway rule,
LF system credential fallback, one-hour cache by kind and folded query, send-once throttle handling,
and a query that is never logged or echoed. No create-side change. A city radius and zip codes stay
out, as the region-targeting entry left them.

Not live-verified: the `adlocale` and `adgeolocation` answer shapes are from Meta's Targeting Search
reference. A read-only probe with the LF system credential is owed before a picker relies on them.

Tests: `targeting_search_test.go` (both request shapes; `key` rows as numbers or strings, a 0 or
leading-zero key, an `id`-only row, a country, a zip and a typeless row dropped; location fields
carried), `meta_targeting_search_test.go` in dispatch (location fields reach the caller, cached under
their kind) and in service (the three fields mapped, absent for other kinds); the decoder test covers
the two new `type` values through `meta.TargetingSearchKinds`.
