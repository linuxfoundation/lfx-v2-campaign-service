# 2026-11-03 — #458 Google conversion actions list for a picker

**Update** — `googleAdsConfig.conversionActions` took ids typed by hand:
`googleads.ListConversionActions` read the account's actions, but nothing served it over HTTP.

`GET /projects/{project_id}/google-ads/conversion-actions` (`list-google-ads-conversion-actions`)
now returns `account_id`, `conversion_actions` (id, resource name, name, status, type, category,
primary-for-goal) and `conversion_action_count`. The path sits with the other create-form
lookups (`geo-targets`, `user-lists`) in the HTTPRoute regex, but is ruled by its own Heimdall
rule, `root-campaign-admin:get`, not `project-api` (see the tenancy bullet).

- `ListConversionActions` now selects ENABLED actions only (it used to keep PAUSED).
- `GoogleAdsDispatcher.ListConversionActions` resolves with `creds.resolve`, as a create does
  (own connection, LF fallback, forced-system redirect), so the ids are ones that create
  accepts. A stored account id that is not digits only is refused before any request: 400 on
  the project's own row, 500 on the LF system row it fell back to.
- `Orchestrator.ReadConversionActions` records a `list_conversion_actions` upstream call only
  when Google was contacted (the dispatcher reports `called`), and
  `ConnectionService.ListGoogleAdsConversionActions` refuses the reserved LF scope and maps
  errors through `classifyTargetingLookupError` (404 no connection, 400 unusable connection,
  500 unusable LF system row, 503 upstream). More than 500 enabled actions is a 503 that does
  not clear on retry.
- **Tenancy, enforced at the gateway:** the read is NOT refused on the shared Google Ads
  customer, unlike user lists, so it lists every foundation's conversion action names. The
  `root-campaign-admin:get` rule (GET only) therefore admits only a cross-foundation principal:
  one `openfga_or_check` allows `writer_guard` OR `global_marketing_ops` on the ROOT project,
  the two checks of lfx-self-serve's `isRootCampaignPrincipal`. A project `campaign_manager` is
  not admitted. The ROOT project's UID is the new chart value `openfga.rootProjectUid`, set per
  environment in lfx-v2-argocd; the gateway cannot resolve it, because project-service's HTTP
  slug-to-uid lookup refuses the slug `ROOT`. Until an environment sets it the rule is
  `deny_all` there (403 for everyone); a non-UUID value fails the render. With OpenFGA disabled
  (local dev) the rule is `allow_all`, as `project-api` does. The Goa
  description, `api-catalog.md`, `docs/architecture.md`, the RuleSet and HTTPRoute concepts and
  the lookups concept say so.

Tests: `TestListConversionActions_*` (GAQL, decoding, empty, missing id, row cap),
`TestGoogleAdsListConversionActions_*` (one `googleAds:search`, system fallback, forced system,
local refusals, a bad LF system row, upstream error), `TestListGoogleAdsConversionActions_*`,
`TestReadConversionActions_NilResultIsAnError` and `_RecordsOnlyCallsThatReachedGoogle`, the
extended lookup error-mapping, system-scope and unsupported-dispatcher tests,
`TestConversionActionsRoute_IsGetOnly`, the parity rows, `TestRootAdminRule*` (uid set, unset,
malformed, OpenFGA disabled) and `TestConversionActionsIsRuledOnlyByTheRootAdminRule`.
