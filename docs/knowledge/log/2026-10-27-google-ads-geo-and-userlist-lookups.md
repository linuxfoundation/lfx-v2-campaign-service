# 2026-10-27 — Google Ads geo target and user-list lookups

**Creation** — two read-only endpoints for the self-serve create form's pickers (self-serve
#4455, #4456): `GET /projects/{project_id}/google-ads/geo-targets` searches Google's geo target
constants (SuggestGeoTargetConstants) and returns the numeric id `geoTargets` accepts, and
`GET /projects/{project_id}/google-ads/user-lists` lists a customer's user lists (GAQL on
`user_list`) and returns the resource name `audienceSegments` accepts. The geo search may use
the LF system credential (public reference data); the user-list read is own-connection only,
like the account monitor, because user lists are account data on a shared customer. Both are
ruled in `ruleset.yaml` and admitted by the HTTPRoute regex. See
[google-ads-targeting-lookups](../architecture/google-ads-targeting-lookups.md).
