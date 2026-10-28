# 2026-10-28 — The shared-customer scope query's comment names both selected columns (#166)

**Docs** — `listProjectPlatformCampaignIDsQuery`'s comment said "Only the platform id is
selected", while the query selects `platform_campaign_id, result`, and the same comment block
explains why `result` must travel. The comment now names both columns. #166's substantive ask, a
live test proving the project boundary instead of a SQL substring match, was already delivered by
`aef68385` (#153): `TestLiveListProjectPlatformCampaignIDsIsTenantScoped` seeds a second project
and asserts the exact id set. See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md).
