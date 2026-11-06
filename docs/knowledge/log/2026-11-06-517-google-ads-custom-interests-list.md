# 2026-11-06 — Custom interests in the Google Ads audience list (#517)

**Update** — `GET /projects/{project_id}/google-ads/audiences` takes a fifth `type`,
`custom_interest`, so a picker can offer what Display's `customAffinities` accepts
(`customers/{id}/customInterests/{id}`). The four #496 kinds had no way to list them, though the
KubeCon NA 2025 Display campaign used two custom affinities.

- `googleads.ListAudiences` (`internal/platform/googleads/audiences.go`) adds
  `AudienceKindCustomInterest`: one GAQL search on `custom_interest` with
  `status = 'ENABLED' AND type = 'CUSTOM_AFFINITY'`, the same 10000-row bound, the
  `customInterests` collection check, and `subtype` = `custom_interest.type`. CUSTOM_INTENT rows
  are not listed: a validateOnly probe on 2026-11-06 (paused LFX Display ad group 210268047788)
  accepted a CUSTOM_AFFINITY resource in a `customAffinity` criterion and refused two
  CUSTOM_INTENT ones with RESOURCE_NOT_FOUND. `customAffinities` itself still checks only the
  collection, so a CUSTOM_INTENT id typed by hand fails at Google after the budget.
- The design `type` enum (three places) and the service's 400 message name the new value. The
  dispatcher needs no change: only `user_interest` is cached, so a custom interest created a
  minute ago is offered at once (`TestGoogleAdsListAudiences_CustomInterestIsNotCached`).
- Gate unchanged: the path is already in the `marketing-ops-api` Heimdall rule
  (`global_marketing_ops`), which keys on the path, not the query.
- A read-only inventory search of the LF customer on 2026-11-06, filtered on ENABLED status but
  not on type, returned 57 rows: 28 CUSTOM_AFFINITY and 29 CUSTOM_INTENT. The shipped picker query
  adds `custom_interest.type = 'CUSTOM_AFFINITY'`, so it offers only the 28; that query itself was
  not run live.
