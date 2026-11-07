# 2026-11-06 — #550 Meta custom audience picker read

**Creation** — `GET /projects/{project_id}/meta-ads/custom-audiences` (`list-meta-custom-audiences`),
gap 3 of the Meta gap review (#550): the read the self-serve audience pickers call to offer ids for
`metaConfig.targeting.customAudiences` and `excludedCustomAudiences`, instead of making an operator
paste raw Meta ids.

It reads the create account's `customaudiences` edge live, all or nothing (20 pages of 100, then 503,
never a silent truncation). Every row must carry an audience `subtype` and the connection's account.
Optional `q` filters by name. Results are sorted by name and capped at 500 with `truncated`. The LF
Meta ad account is shared by every foundation, so the route sits on the gateway's `marketing-ops-api`
rule (`global_marketing_ops`) like `google-ads/audiences`. The chart's RuleSet, HTTPRoute regex and
`parity_test.go` (`TestMetaCustomAudiencesIsAMarketingOpsRead`) change together.

Tests: the client walk, cursor, field list, and ten malformed-answer refusals plus the page bound
(`custom_audiences_test.go`); the dispatcher's account and local refusals; the service's mapping,
filter, sort, cap, empty array, 400s and status mapping; and the upstream-instrumentation case for
`list_meta_custom_audiences`.

Local review fixes before the PR: the read has its own 45-second budget
(`metaCustomAudiencesCallTimeout`), since its walk may make 20 sequential Graph calls; the catalog row
documents the 500 arms of the LF-system fallback; and every "except the Google lists" line in the
authorization docs now names the Meta custom-audience list too.

PR #561 review: an explicitly empty `?q=` is refused with a 400 (`refuseEmptyQueryParam`, as for
`google-ads/audiences`) instead of decoding as "no filter" and returning the unfiltered list;
`TestMetaCustomAudiencesEmptyQIsRefusedByTheRealMux` drives the real mux.
