# 2026-08-05 — Meta metrics reads

**Update** — Added campaign metrics reads for Meta: a new optional `MetricsReader` dispatcher
capability is wired for Meta in `internal/dispatch/meta.go`'s `ReadMetrics`, backed by a new
`internal/platform/meta/metrics.go` Graph API insights client method (`GetCampaignMetrics`):
campaign id and metrics window (fixed allow-list of supported date_preset values, e.g.
`LAST_30_DAYS`) are both validated before string interpolation into the request, since Meta's
insights endpoint has only fixed preset values. Platform-agnostic domain type `model.CampaignMetrics`
(Impressions/Clicks/CostMicros/Ctr) is distinct from the platform-level `meta.CampaignMetrics`,
converted at the dispatcher boundary — mirrors the Google Ads GA-5 pattern exactly. Cost is
expressed in micros of the ad account's currency (multiplying Meta's spend value by 1,000,000),
matching Google Ads' unit so a platform-agnostic dispatcher can normalize all platforms to the same
scale. The existing platform-agnostic `/metrics` endpoint (wired via the orchestrator's optional-capability
type assertion) now works for Meta campaigns, same as Google Ads (LFXV2-2993).
