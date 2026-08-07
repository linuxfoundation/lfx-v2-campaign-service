# 2026-08-05 — LFXV2-2023 GA-3b: status-toggle test data race, api-catalog refresh

**Update** — Follow-up fix on the GA-3b slice recorded in
`2026-08-04-ga3b-adgroup-ad-cascade.md`, from the post-commit review cycle. A
`internal/platform/googleads/adgroup_ad_test.go` subtest for `UpdateAdGroupAndAdStatus`
wrote a captured request-path slice inside the httptest handler goroutine and read it back
from the test goroutine with no happens-before edge — a data race the repo's race detector
(`make test`) would eventually flag. Guarded with a `sync.Mutex`, matching the pattern already
used elsewhere in this package (`campaign_test.go`). `docs/api-catalog.md`'s
`GoogleAdsConfig` section was also refreshed — it still described the pre-GA-3b budget-only
contract and was missing the new `headlines`/`descriptions` fields.
