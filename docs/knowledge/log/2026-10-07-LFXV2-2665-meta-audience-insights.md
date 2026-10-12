# 2026-10-07 — LFXV2-2665 Meta audience insights read

**Update** — New `campaign_manager` read `get-meta-ads-audience`,
`GET /projects/{projectId}/meta-ads/audience`, the Meta sibling of `get-google-ads-audience`.

- **Upstream.** Per breakdown, `GET /act_{id}/insights` with `level=campaign`,
  `fields=campaign_id,impressions,clicks,spend,account_currency`, `breakdowns=age,gender` then
  `breakdowns=publisher_platform,platform_position`, the metrics read's `date_preset` for the
  window, `filtering=[{"field":"campaign.id","operator":"IN","value":[…]}]` over the project's
  OWN campaign ids, `limit=500`, cursor paging bounded at 20 pages (a `next` on the last page
  fails the read). New `internal/platform/meta/audience.go`; the spend parser was extracted from
  `GetCampaignMetrics` into `parseSpendMicros` and is now shared (messages unchanged).
- **Trust.** Every row's campaign must be in scope; duplicate JSON keys, duplicate
  (campaign, segment) rows, inconsistent/invalid `account_currency`, missing breakdown keys and
  values outside `^[A-Za-z0-9][A-Za-z0-9_+\-]{0,63}$` all fail the whole read (503, no partial
  rows, values never echoed). Unknown-but-safe values are kept verbatim.
- **Plumbing.** New optional capability `service.MetaAudienceReader` (only `MetaDispatcher`
  implements it, so Microsoft/Reddit/X/LinkedIn/Google stay `400 not supported`),
  `Orchestrator.ReadMetaAudienceInsights` (empty scope → 200 empty, no upstream call),
  `model.MetaAudienceInsights`/`MetaAudienceBucket`, and two 409 sentinels
  `domain.ErrAudienceScopeTooLarge` (> 250 campaigns, a local URL bound) and
  `domain.ErrAudienceScopeInvalid` (non-canonical stored id). Account mismatch on ANY scoped
  campaign is 409, no connection 404, no `act_<digits>` account selected 400.
- **No conversions field**, deliberately: Meta exposes conversions only per action type.
- Chart: HTTPRoute regex, RuleSet entry and parity rows for `meta-ads/audience`.
- Verified only against Meta's published Marketing API documentation (breakdown combinations,
  `filtering` on `campaign.id`, `account_currency`), not against a live account.
