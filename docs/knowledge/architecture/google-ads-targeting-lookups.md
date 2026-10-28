---
type: "Architecture Doc"
title: "Google Ads Targeting Lookups"
description: "Two read-only pickers for the Google Ads create form: a geo target constant search (SuggestGeoTargetConstants, by name or postal code, returning the numeric id geoTargets accepts, readable through the LF system credential because it is public reference data) and a user-list read (GAQL on user_list, returning the resource name audienceSegments accepts, own connection only, and refused with 409 on a shared-tenancy platform such as Google Ads because the read is account-wide and no user list is recorded per project)."
resource: "internal/service/google_ads_lookups.go"
---

# Google Ads Targeting Lookups

`googleAdsConfig.geoTargets` and `googleAdsConfig.audienceSegments` take raw Google ids.
These two reads let the self-serve create form offer them in a picker instead of making an
operator type them (self-serve #4455, #4456). Neither persists anything.

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
  still gets its 404. A hit is reported as `cached` and records no `suggest_geo_targets` upstream
  call; an empty answer is cached as a non-nil empty slice. The one-shot client uses
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

## Shared

- Authorization: the gateway's `campaign_manager` check on `project:{projectId}`
  (`ruleset.yaml`), JWT verified in the service, the reserved `system:linuxfoundation` scope
  refused with 404 (`rejectSystemScope`).
- Errors: `classifyDiscoveryError` arms under each read's own operation noun, plus
  `ErrTargetingLookupUnsupported` (400) for a dispatcher without the capability
  (`GeoTargetSuggester` / `UserListReader`). Upstream failures are 503, including Google refusing
  a caller-supplied `country_code` or an `account_id` the credential cannot reach (the account
  monitor behaves the same). Input lengths are counted in characters, matching the Goa decoder.
- Metrics: `suggest_geo_targets` and `list_user_lists` upstream-call operations.
