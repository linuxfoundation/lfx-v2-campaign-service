# 2026-11-14 — #550 Meta targeting search picker read (gap 7)

**Creation** — The picker read gap 7 of the Meta gap review (#550) left for later. Interests,
behaviors and work positions are sent as Meta ids, and nothing let the UI find them by name.
`GET /projects/{p}/meta-ads/targeting-search?type=<interest|behavior|work_position>&query=`
(`search-meta-targeting-options`) proxies Meta's Targeting Search: `adinterest` and `adworkposition`
with `q`, and the `adTargetingCategory` class `behaviors` browse, which ignores `q`, so behaviors
are filtered by name here. It returns `{type, options: [{id, name, path?, description?,
audience_size_lower_bound?, audience_size_upper_bound?, coverage?}], option_count}`, at most 50.

Gated like LinkedIn's typeahead (#585): Meta's taxonomy names no ad account resource, so it is a
`campaign_manager` read on the gateway's `project-api` rule (parity test rows and
`TestMetaTargetingSearchIsACampaignManagerRead`), and credentials resolve as for a create, the LF
system account included, with no ad account needed. The query is 2–100 characters with a
control-character-free pattern, re-checked by the service and the client, and is never logged or
echoed: errors carry only the kind and a failure class, with the query cut off every URL in the
chain. Answers are cached for an hour (behaviors as one browse). The request is sent once, so a
throttle answers 503 instead of being retried.

Behind `META_TARGETING_SEARCH_ENABLED` (default off; 400 while off). Not live-verified: the answer
shapes of all three searches.

Tests: `targeting_search_test.go` (request shapes, row parsing, behavior filtering, local refusals,
send-once and no query in any error), `meta_targeting_search_test.go` in dispatch (cache by query
and by browse, TTL, local refusals, failures not cached) and in service (field mapping, 400s before
dispatch, error classes), `meta_targeting_search_decode_test.go` (the decoder mirrors the platform),
the instrumentation row and the chart parity rows. Twenty-nine hand mutations each fail a test.
