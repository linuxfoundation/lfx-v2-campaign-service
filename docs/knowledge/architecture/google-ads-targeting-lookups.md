---
type: "Architecture Doc"
title: "Google Ads Targeting Lookups"
description: "Three read-only pickers for the Google Ads create form: a geo target constant search (SuggestGeoTargetConstants, by name or postal code, returning the numeric id geoTargets accepts, readable through the LF system credential because it is public reference data), a user-list read (GAQL on user_list, returning the resource name audienceSegments accepts, own connection only, and refused with 409 on a shared-tenancy platform such as Google Ads because the read is account-wide and no user list is recorded per project), and a conversion-action read (GAQL on conversion_action, ENABLED only, resolved as a create resolves; it lists every foundation's actions on the shared customer, so the gateway admits only ROOT writer_guard or ROOT global_marketing_ops holders, not a project campaign_manager)."
resource: "internal/service/google_ads_lookups.go"
---

# Google Ads Targeting Lookups

`googleAdsConfig.geoTargets`, `googleAdsConfig.audienceSegments` and
`googleAdsConfig.conversionActions` take raw Google ids. These three reads let the self-serve
create form offer them in a picker instead of making an operator type them (self-serve #4455,
#4456; campaign-service #458). None persists anything.

## Geo target search

`GET /projects/{project_id}/google-ads/geo-targets?q=&country_code=&target_types=&limit=`
(`suggest-google-ads-geo-targets`).

- Upstream: `POST /v23/geoTargetConstants:suggest` (GeoTargetConstantService.SuggestGeoTargetConstants)
  with `locale=en`, the optional country and `locationNames.names=[q]`. Account-agnostic, so the
  client is the discovery client (no customer id; the login-customer-id header is still validated).
- `id` is the **numeric** constant id, in exactly the form `resolveGeoList`
  (`internal/platform/googleads/geo.go`) accepts. The `geoTargetConstants/{id}` resource name
  is **not** accepted by that validator, so it is not returned. Rows whose id is not canonical are
  dropped; duplicate ids are collapsed.
- `target_types` filters Google's label (`City`, `Postal Code`, ...) ignoring case, spaces,
  underscores and hyphens (`googleads.NormalizeGeoTargetType`). Google's suggest has no type filter
  of its own, so the filter runs after the call; `limit` (1-50, default 20 applied by the service; no Goa `Default`, so the generated client
  omits an unset limit) caps the filtered rows
  and `truncated` reports more.
- **Resolution: the LF system fallback is allowed** (`resolveGoogleAdsDiscoveryClient`, as for
  `/connection-google-ads/accounts`). The answer is Google's public location table and reveals
  no ad account's data.
- **Cached** per dispatcher (`geoSuggestCache`: 6h TTL, 2048 entries, keyed by country + folded
  query) so a typeahead does not spend the shared developer token's quota on repeated searches.
  The cache is consulted after credential resolution, so a project with no reachable credential
  still gets its 404. The dispatcher reports whether Google was reached (`called`), and only a
  request that went out records a `suggest_geo_targets` upstream call, never a cache hit or a
  local refusal; an empty answer is cached as a non-nil empty slice. The one-shot client uses
  `WithCallerScopedTokenRefresh`.

## User lists

`GET /projects/{project_id}/google-ads/user-lists?account_id=` (`list-google-ads-user-lists`).

- Upstream: GAQL `SELECT user_list.resource_name, id, name, type, membership_status,
  account_user_list_status, size_for_search, size_range_for_search, eligible_for_search FROM
  user_list ORDER BY user_list.id LIMIT 501` on the customer (bounded, deterministic), sorted by
  name locally, capped at 500 with `truncated`.
- `resource_name` is `customers/{id}/userLists/{id}`, exactly what `validateAudienceSegments`
  accepts. A row naming another customer, or a non-userLists resource, is dropped.
- `account_id` (digits only) picks the customer; omitted, the connection's stored account is
  read. No account either way is a 400 (`ErrConnectionNotUsable` + `ErrAccountNotSelected`).
- **Refused with 409 on a shared-tenancy platform** (`domain.ErrUserListsSharedTenancy`), before
  any credential is resolved. `Orchestrator.ReadUserLists` keys on `service.AccountMonitorScoper`,
  the codebase's shared-tenancy marker, which the Google Ads dispatcher implements. Refusing the
  LF fallback is not enough: per `docs/architecture.md` ("Account Tenancy") each project's OWN
  connection row points at the one shared customer, so an account-wide `user_list` read hands
  every foundation's audiences to any project `campaign_manager`. The account monitor's remedy
  (filter to ids recorded for the project) has nothing to filter by, since no user list is
  recorded per project, and the BFF's ROOT-only gate (lfx-self-serve-ops#54) is bypassed by a
  direct call. Until a per-project list mapping exists, the picker falls back to manual entry
  on Google Ads; the read itself remains for a per-foundation customer.
- **Resolution, where it runs: own connection only** (`resolveOwnedGoogleAdsDiscovery`, caller-scoped
  token refresh), never the LF system fallback; a project with no connection of its own gets 404.

## Conversion actions

`GET /projects/{project_id}/google-ads/conversion-actions` (`list-google-ads-conversion-actions`,
#458).

- Upstream: one GAQL search, `SELECT conversion_action.id, resource_name, name, status, type,
  category, primary_for_goal FROM conversion_action WHERE conversion_action.status = 'ENABLED'
  ORDER BY conversion_action.name` (`googleads.ListConversionActions`). REMOVED and PAUSED
  actions are not listed. More than 500 rows fails the read (503) rather than truncating it, and
  a row without its id or resource name fails it too.
- `id` and `resource_name` are the two spellings `validateConversionActions` accepts.
- **Resolution: as a create resolves** (`creds.resolve`, then the cached account-bound client):
  the project's own connection, else the LF system fallback, and the LF account whenever the
  deployment forces it. A conversion action belongs to one customer and the create refuses one
  owned by another, so the list must come from the account the create will use. The stored
  account id is checked for digits only before any request (400 when it is not).
- **ROOT principals only, enforced at the gateway.** Unlike user lists, this read is NOT refused
  on a shared-tenancy platform. Google Ads is one customer shared by every foundation, so the
  answer holds every foundation's conversion action names, and the read exists for
  cross-foundation (LF admin) callers. It is therefore ruled by its own Heimdall rule,
  `root-campaign-admin:get`, not `project-api`: `project_slug_resolver_contextualizer` resolves
  the fixed slug `ROOT`, and one `openfga_or_check` admits `writer_guard` OR
  `global_marketing_ops` on that project — the same two checks as lfx-self-serve's
  `isRootCampaignPrincipal`. A `campaign_manager` of the path's project is not admitted, and
  `{project_id}` only selects whose connection supplies the credential. GET only. With OpenFGA
  disabled (local dev) the rule is `allow_all`, like every rule. User lists have no such gate,
  which is why that read refuses the shared account instead.

## Shared

- Authorization: the gateway's `campaign_manager` check on `project:{projectId}`
  (`ruleset.yaml`) for geo targets and user lists, the ROOT check above for conversion actions;
  JWT verified in the service, the reserved `system:linuxfoundation` scope
  refused with 404 (`rejectSystemScope`).
- Errors: `classifyDiscoveryError` arms under each read's own operation noun, plus
  `ErrTargetingLookupUnsupported` (400) for a dispatcher without the capability
  (`GeoTargetSuggester` / `UserListReader` / `ConversionActionReader`). Upstream failures are
  503, including Google refusing a caller-supplied `country_code` or an `account_id` the
  credential cannot reach (the account monitor behaves the same). Input lengths are counted in
  characters, matching the Goa decoder.
- Metrics: `suggest_geo_targets`, `list_user_lists` and `list_conversion_actions` upstream-call
  operations.
