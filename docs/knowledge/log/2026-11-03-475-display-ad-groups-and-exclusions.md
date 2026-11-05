# 2026-11-03 — #475 Display ad groups and audience exclusions

**Update** — The NA 2025 Display reference campaign runs two ad groups, Retargeting and
Prospecting, each with its own audiences. After #462 a Display create here built one ad group
and could not exclude an audience.

Both shapes were checked by validateOnly probes on 2026-11-03 at v23. Two `DISPLAY_STANDARD`
ad groups under one Display campaign, each with its own audience criteria, were accepted. A
negative user list was accepted on a Display ad group (`{adGroup, userList:{userList},
negative:true}`) and on the campaign (`{campaign, userList:{userList}, negative:true}`).

`googleAdsConfig.displayAdGroups` maps to `CampaignInput.DisplayAdGroups`
(`display_adgroups.go`): up to five named groups, each with a required responsive display ad,
the four #462 audience lists and its own `excludedAudienceSegments`. Groups are named
`<event> - Display Network | <label>`, held to 255 characters and unique ignoring case.
`googleAdsConfig.excludedAudienceSegments` maps to `CampaignInput.ExcludedAudienceSegments`,
user lists excluded from the whole campaign. Only user lists can be excluded, under the same
shape and account check as #462's lists (account-scoped, not tenancy protection; #468). A
list both targeted and excluded, or excluded at both levels, is refused. The campaign-level
creative and audience lists must be empty beside the list. Both fields are refused off Display,
and Search's `adGroups` on Display now names `displayAdGroups`.

The cascade fetches every group's images in one pass before the budget, under one 64 MiB
total. After the campaign criteria it creates each group's ad group (with the AUDIENCE
targeting setting when that group has audiences), its audience criteria and its exclusions;
then the campaign-level placement and user-list exclusions; then each group's assets and ad,
so every criteria step still runs before any ad. With the list, `AdGroups` reports each group
and the scalar fields are the first group's. Without it the requests and result are unchanged.

`googleAdsCreativeSlots` and the snapshot sanitising cover every group's image slots, and the
creative-assets concept no longer states a fixed slot count. The api-catalog Google Ads
entries and the googleads and dispatch concepts are updated.

Tests: `display_adgroups_test.go` (payloads and mutate order, the per-group targeting
setting, every refusal, partial and UNCONFIRMED results, the context between groups, the
shared 64 MiB total) and `googleads_display_adgroups_test.go` (mapping, the campaign-level
exclusion through Dispatch, refusals before the budget, snapshot and slot labels).
