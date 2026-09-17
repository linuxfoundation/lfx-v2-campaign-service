# 2026-09-17 monitor account endpoints

**Update** — Added four account-scoped monitor endpoints
(`GET /projects/{project_id}/connection-{platform}-ads/account-monitor`)
porting the LFX One BFF's `/api/campaigns/*/monitor` family for Google,
LinkedIn, Meta, and Reddit: new `AccountMetricsReader` orchestrator
capability, four dispatcher implementations, four ported rule engines
(deliberately not unified onto the shared `rules` package — see
[Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md)),
and the `monitorAccount` service-layer handler. Meta's dispatcher paginates
fully rather than porting the legacy BFF's silent 100-campaign truncation.

**Fix** — Local OLD-vs-NEW differential verification against the live BFF
(Google only so far) found and fixed three real port defects: `googleads`
account-monitor query missing the channel-type/status/impressions scope
filters `getMonitorData` applies (query was returning years of `REMOVED`
campaign history instead of the current 23 active ones); the totals fallback
summing pre-rule-engine rows instead of the post-filter rows the response
actually returns (`campaign_count` disagreed with `len(campaigns)`); and
`AccountMonitorTotals.conversions` declared `Int64` in the Goa design and
domain model, truncating each row's fractional conversions before summing
(now `Float64`, matching the per-campaign attribute and the BFF's plain
float sum).
