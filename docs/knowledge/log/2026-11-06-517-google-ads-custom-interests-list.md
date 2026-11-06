# 2026-11-06 — Custom interests in the Google Ads audience list (#517)

**Update** — `GET /projects/{project_id}/google-ads/audiences` takes a fifth `type`,
`custom_interest`, so a picker can offer what Display's `customAffinities` accepts
(`customers/{id}/customInterests/{id}`). The four #496 kinds had no way to list them, though the
KubeCon NA 2025 Display campaign used two custom affinities.

- `googleads.ListAudiences` (`internal/platform/googleads/audiences.go`) adds
  `AudienceKindCustomInterest`: one GAQL search on `custom_interest` with
  `status = 'ENABLED'`, the same 10000-row bound, the `customInterests` collection check, and
  `subtype` = `custom_interest.type` (CUSTOM_AFFINITY or CUSTOM_INTENT). Both types are listed,
  because `customAffinities` checks only the collection.
- The design `type` enum (three places) and the service's 400 message name the new value. The
  dispatcher needs no change: only `user_interest` is cached, so a custom interest created a
  minute ago is offered at once (`TestGoogleAdsListAudiences_CustomInterestIsNotCached`).
- Gate unchanged: the path is already in the `marketing-ops-api` Heimdall rule
  (`global_marketing_ops`), which keys on the path, not the query.
- A read-only search of the LF customer on 2026-11-06, with the exact query, returned 57
  enabled rows: 28 CUSTOM_AFFINITY and 29 CUSTOM_INTENT.
