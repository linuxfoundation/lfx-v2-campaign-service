# 2026-11-05 — Google Ads audience list for the audience pickers (#496)

**Creation** — `GET /projects/{project_id}/google-ads/audiences?type=&q=`
(`list-google-ads-audiences`) lists the custom audiences, combined audiences, Audience resources
or user interests (one `type` per call) on the Google Ads customer a create for this project
uses, so the self-serve Display, Demand Gen and Performance Max audience pickers can offer them
instead of making an operator paste resource names.

- `googleads.ListAudiences` (`internal/platform/googleads/audiences.go`) runs one GAQL search per
  kind, usable entries only: `custom_audience`, `combined_audience` and `audience` with status
  ENABLED; `audience` with scope CUSTOMER as well; `user_interest` with taxonomy AFFINITY or
  IN_MARKET. It is bounded at 10000 rows, and more than that fails the read. Rows outside this
  customer's collection are dropped, and the rest are sorted by name ignoring case.
- `GoogleAdsDispatcher.ListAudiences` resolves the way `ListConversionActions` does. The shared
  resolution is now `createAccountClient`: the project's own connection, the LF fallback, and
  the forced-system redirect.
- The service validates `type` and `q`, filters by name ignoring case, then caps at 500 with
  `truncated`. Rows carry `subtype` (Google's custom audience type or taxonomy type) and omit
  `status` for `user_interest`, which has none. `list_audiences` is recorded only when Google
  was contacted.
- Tenancy: the answer lists every foundation's audiences on the shared customer, so the path is
  in the `marketing-ops-api` Heimdall rule (`global_marketing_ops`), not `project-api`, and the
  HTTPRoute regex forwards it. `TestGoogleAudiencesIsAMarketingOpsRead` and
  `TestMarketingOpsRuleHoldsExactlyItsReads` (`charts/lfx-v2-campaign-service/parity_test.go`)
  pin the path and the rule's whole path set. The conversion-action test now shares the
  `assertMarketingOpsRead` helper.
- Read-only GAQL on the LF customer (2026-11-05) returned 85 enabled custom audiences, 0 combined
  audiences, 53 CUSTOMER-scoped Audiences (10 more are ASSET_GROUP-scoped), and 1008
  AFFINITY + IN_MARKET user interests. `user_interest.launched_to_all` was false on every one of
  those rows, so the read does not filter on it.

The user-list read (`google-ads/user-lists`) is unchanged: it stays on `project-api` and is
refused with 409 on the shared account.
