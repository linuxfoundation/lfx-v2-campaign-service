---
type: "Architecture Doc"
title: "Google Ads Targeting Lookups"
description: "Four read-only pickers for the Google Ads create form: a geo target constant search (SuggestGeoTargetConstants, by name or postal code, returning the numeric id geoTargets accepts, readable through the LF system credential because it is public reference data), a user-list read (GAQL on user_list, returning the resource name audienceSegments accepts, resolved as a create resolves; it lists every foundation's user lists on the shared customer, so it is gated on global_marketing_ops), a conversion-action read (GAQL on conversion_action, ENABLED only, resolved as a create resolves; it lists every foundation's actions on the shared customer, so the gateway admits only global_marketing_ops, the LF marketing team, not campaign_manager, which also includes foundation executive directors), and an audience read (GAQL on custom_audience, combined_audience, audience, user_interest or custom_interest, one type per call, usable entries only, optional name search, resolved and gated like the conversion-action read)."
resource: "internal/service/google_ads_lookups.go"
---

# Google Ads Targeting Lookups

`googleAdsConfig.geoTargets`, `googleAdsConfig.audienceSegments`,
`googleAdsConfig.conversionActions` and the audience inputs (`customAudiences`, `userInterests`,
`demandGenAudience`, `performanceMaxAudienceSignal`, `customAffinities`) take raw Google ids. These four reads let the self-serve
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
  accepts (Search; on Display, `validateDisplayAudienceList`, which also requires the
  campaign's account). A row naming another customer, or a non-userLists resource, is dropped.
- `account_id` (digits only) picks the customer; omitted, the account a create for this project
  uses is read. A malformed `account_id` is a 400 before any credential is resolved.
- **Resolution: as a create resolves** (`createAccountClient`, shared with the conversion-action
  and audience reads): the project's own connection, else the LF system fallback, and the LF
  account whenever the deployment forces it, so the lists are on the account the create uses. A
  connection with no account selected or a non-digit stored id is refused as a connection to fix
  (400 on the project's row, 500 on the LF row), even when `account_id` names another customer.
- **A `global_marketing_ops` read.** On the shared Google Ads customer the answer is every
  foundation's user lists: 450 on the LF customer on 2026-11-05 (84 CRM_BASED, 110 RULE_BASED,
  89 REMARKETING, 151 SIMILAR, 15 LOGICAL, 1 LOOKALIKE; 351 OPEN; 378 eligible for Search). Until
  2026-11-05 this was a `campaign_manager` read, so it resolved the project's own connection
  only and `Orchestrator.ReadUserLists` refused a shared-tenancy platform with 409
  (`ErrUserListsSharedTenancy`): nothing scopes user lists per project, and a foundation
  executive director would have seen every foundation's lists. The route moved to the
  `marketing-ops-api` rule (the LF marketing team only), the same population as the
  conversion-action and audience reads, so both refusals and the 409 response were removed and
  the Search observed-lists picker can list the LF account's HubSpot CRM and website visitor
  lists.

## Conversion actions

`GET /projects/{project_id}/google-ads/conversion-actions` (`list-google-ads-conversion-actions`,
#458).

- Upstream: one GAQL search, `SELECT conversion_action.id, resource_name, name, status, type,
  category, primary_for_goal FROM conversion_action WHERE conversion_action.status = 'ENABLED'
  ORDER BY conversion_action.name` (`googleads.ListConversionActions`). REMOVED and PAUSED
  actions are not listed. More than 500 rows fails the read (503) rather than truncating it, and
  that 503 does not clear on retry: the account holds more enabled actions than the read
  publishes. A row without its id or resource name fails the read too.
- `id` and `resource_name` are the two spellings `validateConversionActions` accepts.
- **Resolution: as a create resolves** (`creds.resolve`, then the cached account-bound client):
  the project's own connection, else the LF system fallback, and the LF account whenever the
  deployment forces it. A conversion action belongs to one customer and the create refuses one
  owned by another, so the list must come from the account the create will use. The stored
  account id is checked for digits only before any request: 400 on the project's own row, and
  500 on the LF system row it fell back to (`ErrSystemConnectionNotUsable`), which only an
  operator can fix.
- **A `global_marketing_ops` read, not a `campaign_manager` one.** Like the user-list read, this
  read is NOT refused on a shared-tenancy platform. Google Ads is one customer shared by every
  foundation, so the answer holds every foundation's conversion action names, and
  `{project_id}` only selects whose connection supplies the credential. `campaign_manager`
  includes the project's `executive_director`, a foundation-scoped role, so the gateway admits
  only `global_marketing_ops` of the project: the LF marketing team, granted on every project
  (#486). It was
  first ruled by a ROOT-only Heimdall rule (`root-campaign-admin:get`, keyed on a chart value
  `openfga.rootProjectUid` that no environment ever set, so it answered 403 to everyone); that
  rule and the value were removed and the path moved into its own `marketing-ops-api` rule.
  GET only. The user-list read is gated the same way since 2026-11-05.

## Audiences

`GET /projects/{project_id}/google-ads/audiences?type=&q=` (`list-google-ads-audiences`, #496).

- `type` is required: `custom_audience`, `combined_audience`, `audience`, `user_interest` or
  `custom_interest` (#517),
  spelled exactly (no case folding). One GAQL search on the resource of the same name
  (`googleads.ListAudiences`), bounded at `LIMIT 10001`; more than 10000 rows fails the read
  (503) rather than name-searching a partial set.
  - `custom_audience`: `status = 'ENABLED'`; `subtype` is `custom_audience.type` (AUTO, INTEREST,
    PURCHASE_INTENT, SEARCH). Feeds `customAudiences` (Display).
  - `combined_audience`: `status = 'ENABLED'`; no subtype. No create input takes one yet.
  - `audience`: `status = 'ENABLED' AND scope = 'CUSTOMER'`. An ASSET_GROUP-scoped Audience
    belongs to the one asset group it was made for. Feeds `demandGenAudience` and
    `performanceMaxAudienceSignal`.
  - `user_interest`: `taxonomy_type IN ('AFFINITY', 'IN_MARKET')`; `subtype` is the taxonomy
    type. Google's `user_interest` has no status, so none is reported. `launched_to_all` is not
    filtered on: a read-only search of the LF customer on 2026-11-05 returned it false for all
    1008 AFFINITY and IN_MARKET rows. Feeds `userInterests` (Display).
  - `custom_interest` (#517): `status = 'ENABLED'`; `subtype` is `custom_interest.type`
    (CUSTOM_AFFINITY, CUSTOM_INTENT). Google's legacy custom affinity / custom intent segment,
    `customers/{id}/customInterests/{id}`. Feeds `customAffinities` (Display, campaign-level and
    each `displayAdGroups` entry), which accepts either type, so both are listed. A read-only
    search of the LF customer on 2026-11-06 returned 57 enabled rows: 28 CUSTOM_AFFINITY, 29
    CUSTOM_INTENT. Not cached, like the other account kinds.
- Rows whose resource name is not `customers/{this customer}/{collection}/{canonical id}` are
  dropped (the create would refuse them). Sorted by name ignoring case, then resource name.
- `q` (optional, 1-100 characters, not blank) keeps names containing it, ignoring case and
  surrounding space; the service applies it, then caps at 500 with `truncated`. An explicit
  `?q=` is a 400 like a blank q: Goa decodes an empty optional query value as absent, so
  `buildMux` wraps this handler in `refuseEmptyQueryParam` (`cmd/campaign-service`), which
  hands the decoder a blank value for its Pattern to refuse. With 1008 user
  interests on the LF customer, a `user_interest` picker should search rather than browse.
- **`user_interest` is cached** per account (`userInterestCache`: 6h TTL, 64 entries), so a
  typeahead does not run a full search per keystroke. It is consulted after credential
  resolution, a hit reports `called=false` (no `list_audiences` metric) and returns a copy. The
  account kinds are not cached, so a just-created audience is offered at once.
- **Resolution and tenancy: the conversion-action read's.** `createAccountClient` (shared with
  `ListConversionActions`) resolves as a create does, so the resource names are on the account
  the create uses. The answer is account-wide on the shared customer (every foundation's custom
  audiences and Audiences), so the route is in the `marketing-ops-api` rule
  (`global_marketing_ops`), not `project-api`. GET only.
- The singular `google-ads/audience` is a different endpoint: the `campaign_manager` audience
  performance read of the project's own campaigns.

## Shared

- Authorization: the gateway's check on `project:{projectId}` (`ruleset.yaml`):
  `campaign_manager` for the geo read, `global_marketing_ops` for the user-list,
  conversion-action and audience reads;
  JWT verified in the service, the reserved `system:linuxfoundation` scope
  refused with 404 (`rejectSystemScope`).
- Errors: `classifyDiscoveryError` arms under each read's own operation noun, plus
  `ErrTargetingLookupUnsupported` (400) for a dispatcher without the capability
  (`GeoTargetSuggester` / `UserListReader` / `ConversionActionReader` / `AudienceReader`). Upstream failures are
  503, including Google refusing a caller-supplied `country_code` or an `account_id` the
  credential cannot reach (the account monitor behaves the same). Input lengths are counted in
  characters, matching the Goa decoder.
- Metrics: `suggest_geo_targets`, `list_user_lists`, `list_conversion_actions` and
  `list_audiences` upstream-call operations. `suggest_geo_targets`, `list_conversion_actions`
  and `list_audiences` are recorded only when Google
  was contacted (the dispatcher's `called`), never for a local refusal; `list_user_lists` is
  recorded for every call.
