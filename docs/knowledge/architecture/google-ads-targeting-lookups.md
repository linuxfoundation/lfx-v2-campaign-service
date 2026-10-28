---
type: "Architecture Doc"
title: "Google Ads Targeting Lookups"
description: "Two read-only pickers for the Google Ads create form: a geo target constant search (SuggestGeoTargetConstants, by name or postal code, returning the numeric id geoTargets accepts, readable through the LF system credential because it is public reference data) and a user-list read (GAQL on user_list, returning the resource name audienceSegments accepts, own connection only because user lists are account data on a shared customer)."
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
  of its own, so the filter runs after the call; `limit` (1-50, default 20) caps the filtered rows
  and `truncated` reports more.
- **Resolution: the LF system fallback is allowed** (`resolveGoogleAdsDiscoveryClient`, as for
  `/connection-google-ads/accounts`). The answer is Google's public location table and reveals
  no ad account's data.

## User lists

`GET /projects/{project_id}/google-ads/user-lists?account_id=` (`list-google-ads-user-lists`).

- Upstream: GAQL `SELECT user_list.resource_name, id, name, type, membership_status,
  account_user_list_status, size_for_search, size_range_for_search, eligible_for_search FROM
  user_list` on the customer, sorted by name locally, capped at 500 with `truncated`.
- `resource_name` is `customers/{id}/userLists/{id}`, exactly what `validateAudienceSegments`
  accepts. A row naming another customer, or a non-userLists resource, is dropped.
- `account_id` (digits only) picks the customer; omitted, the connection's stored account is
  read. No account either way is a 400 (`ErrConnectionNotUsable` + `ErrAccountNotSelected`).
- **Resolution: own connection only** (`resolveOwnedGoogleAdsDiscovery`), the account monitor's
  rule. User lists are account data, and Google Ads is one customer shared by every foundation, so
  through the fallback any unconnected project would see every foundation's audiences. That is
  why the BFF gates shared-account reads to ROOT (lfx-self-serve-ops#54); this service has no ROOT
  notion and refuses the fallback for every caller instead (404 for a project with no connection
  of its own). Lists cannot be project-scoped (this service records none), so a project with its
  own connection sees what its credential reaches, including, through `account_id`, any other
  customer under the same manager (the Google managed-account guard is deferred, as for the monitor).

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
