# 2026-08-04 — GA-5: campaign metrics reads

**Update** — Added campaign metrics reads (GA-5): a new OPTIONAL `MetricsReader` dispatcher
capability (`internal/service/orchestrator.go`, mirrors `StatusToggler`'s type-assert-and-fall-back
shape) plus `Orchestrator.ReadCampaignMetrics`, a pure read never persisted — unlike
`ToggleCampaignStatus`, no DB write and no If-Match. Google Ads implements it
(`internal/dispatch/googleads.go`'s `ReadMetrics`, backed by a new
`internal/platform/googleads/metrics.go` GAQL `googleAds:search` client method): campaign id
(digit-only) and the metrics window (fixed allow-list of GAQL predefined date-range literals, e.g.
`LAST_30_DAYS`) are both validated before string concatenation into the query, since GAQL has no
parameterized queries. New domain type `model.CampaignMetrics` (platform-agnostic:
Impressions/Clicks/CostMicros/Ctr) is distinct from the platform-level
`googleads.CampaignMetrics`, converted at the dispatcher boundary — same duplication pattern as
`model.Campaign` vs. platform-specific create shapes. Added a new Goa endpoint,
`GET /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/metrics`
(`design/brief.go`'s `get-campaign-metrics`), handled by `BriefService.GetCampaignMetrics`:
`ErrMetricsUnsupported`→400, `ErrCampaignNotProvisioned`→409, any other platform failure→503 (no
UNCONFIRMED case — a read has no partial-apply ambiguity). Bounded by a new 20s
`metricsCallTimeout` (shorter than the toggle path's 45s: a metrics read is a single upstream
query, no cascade to child resources). UNVERIFIED ASSUMPTION carried in
`internal/platform/googleads/metrics.go`: a `segments.date` WHERE-filter without `segments.date`
in SELECT returns one row aggregated over the whole window, not one row per day — not yet checked
against a live account with >1 day of data in the window. No other platform implements
`MetricsReader` yet.
