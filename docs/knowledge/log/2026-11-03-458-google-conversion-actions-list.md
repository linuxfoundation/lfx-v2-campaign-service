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
  accepts. A stored account id that is not digits only is a 400 before any request.
- `Orchestrator.ReadConversionActions` records a `list_conversion_actions` upstream call, and
  `ConnectionService.ListGoogleAdsConversionActions` refuses the reserved LF scope and maps
  errors through `classifyTargetingLookupError` (404 no connection, 400 unusable connection,
  500 unusable LF system row, 503 upstream).
- **Tenancy, enforced at the gateway:** the read is NOT refused on the shared Google Ads
  customer, unlike user lists, so it lists every foundation's conversion action names. The
  `root-campaign-admin:get` rule (GET only) therefore admits only a cross-foundation principal:
  `project_slug_resolver_contextualizer` resolves the fixed slug `ROOT`, and one
  `openfga_or_check` allows `writer_guard` OR `global_marketing_ops` on it, the two checks of
  lfx-self-serve's `isRootCampaignPrincipal`. A project `campaign_manager` is not admitted.
  With OpenFGA disabled (local dev) the rule is `allow_all`, like every rule. The Goa
  description, `api-catalog.md`, `docs/architecture.md`, the RuleSet and HTTPRoute concepts and
  the lookups concept say so.

Tests: `TestListConversionActions_*` (GAQL, decoding, empty, missing id, row cap),
`TestGoogleAdsListConversionActions_*` (one `googleAds:search`, system fallback, forced system,
local refusals, upstream error), `TestListGoogleAdsConversionActions_*` and
`TestReadConversionActions_NilResultIsAnError`, the extended lookup error-mapping, system-scope
and unsupported-dispatcher tests, `TestConversionActionsRoute_IsGetOnly`, the parity rows,
`TestRootAdminRuleEnforcesRootPrincipal` and `TestConversionActionsIsRuledOnlyByTheRootAdminRule`.
