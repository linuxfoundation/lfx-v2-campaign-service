# 2026-11-13 — #585 LinkedIn targeting typeahead

**Update** — `GET /projects/{id}/linkedin-ads/targeting-entities?facet=<facet>&query=<text>`
(`search-linkedin-ads-targeting-entities`) lets self-serve (lfx-self-serve#5545) pick
`linkedInConfig.targeting` values and `geoTargets` by name instead of pasting URNs. It proxies
LinkedIn's `GET /rest/adTargetingEntities?q=typeahead` (LinkedIn-Version 202602) and returns
`{facet, entities: [{urn, name}], entity_count}`.

- Facets: `skills`, `groups`, `titles`, `interests`, `industries`, `employers` (the targeting keys
  LinkedIn's typeahead finder serves) and `locations` (for `geoTargets`). Only URNs of the shape a
  create accepts are returned; the targeting patterns are `targetingFacets`' own.
- Gated on `campaign_manager` (`project-api` rule), not `global_marketing_ops`: the answer is
  LinkedIn's taxonomy, the same for every token, and names no ad-account resource. Credentials
  resolve as for a create, LF system fallback included; no ad account need be selected.
- Answers are cached 6 h per facet and query; the call is bounded at 10 s; the query (1-100
  characters, no control characters) is never logged, and a transport error's URL has its query
  string cut off.

Built from the LinkedIn Marketing API docs (Ad Targeting, li-lms-2026-02). Not yet probed live.
