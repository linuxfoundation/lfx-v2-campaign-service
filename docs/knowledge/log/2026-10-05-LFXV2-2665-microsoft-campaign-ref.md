# 2026-10-05 — LFXV2-2665 Microsoft campaign-ref lookup

**Creation** — Added `GET /projects/{projectId}/microsoft-ads/campaign-ref`
(`resolve-microsoft-ads-campaign`, `campaign_manager`), the Microsoft twin of
`google-ads/campaign-ref`. It maps one Microsoft CampaignId — the `campaign_id` on a
`microsoft-ads/keywords` row — to this service's own campaign and brief, which is what the
Optimize tab needs to reach the Microsoft keyword levers (`keyword-actions`,
`negative-keywords`) from a keyword row.

- A separate method rather than a platform path parameter on the Google route, so the Google
  route, its generated client and its HTTPRoute/RuleSet entries are unchanged. Same payload,
  same `platform-campaign-resolution` result and same errors; both methods call one helper,
  `resolvePlatformCampaignRef`, with the platform fixed by the route. `validateGoogleAdsCampaignID`
  is renamed `validatePlatformCampaignID` (both platforms' ids are positive int64s).
- Trust boundary unchanged: a project-scoped read of this service's `campaigns` table
  (`CampaignRepo.ResolvePlatformCampaign`), never the platform.
- More than one match is reachable for Microsoft (000020's unique index is Google-only, as
  Microsoft ids are per ad account). The `platform-campaign-resolution` type's `matches`
  description now says so; the Google answer is unaffected.
- Chart: RuleSet entry and HTTPRoute regex (`microsoft-ads/(keywords|campaign-ref)`), with
  parity rows for the path and a rejected sub-path.
- Docs: api-catalog row; `microsoft-keyword-insights`, `httproute` and `ruleset` concepts.
- Tests: pair returned, each route resolves only its own platform for the same digits, another
  project's row and a soft-deleted row resolve to nothing, two live rows both returned, malformed
  ids 400, system scope 404, storage fault 500 and cold start 503.
