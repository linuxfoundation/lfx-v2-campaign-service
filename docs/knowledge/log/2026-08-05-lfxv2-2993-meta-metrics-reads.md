# 2026-08-05 — Meta metrics reads

**Update** — Added campaign metrics reads for Meta: a new optional `MetricsReader` dispatcher
capability is wired for Meta in `internal/dispatch/meta.go`'s `ReadMetrics`, backed by a new
`internal/platform/meta/metrics.go` Graph API insights client method (`GetCampaignMetrics`):
campaign id and metrics window (fixed allow-list of supported date_preset values, e.g.
`LAST_30_DAYS`) are both validated before string interpolation into the request, since Meta's
insights endpoint has only fixed preset values. Platform-agnostic domain type `model.CampaignMetrics`
(Impressions/Clicks/CostMicros/Ctr) is distinct from the platform-level `meta.CampaignMetrics`,
converted at the dispatcher boundary. Cost is expressed in micros of the ad account's currency
(multiplying Meta's spend value by 1,000,000), matching the unit `model.CampaignMetrics.CostMicros`
declares so the field means the same thing for every platform rather than silently switching scale
per adapter. The existing platform-agnostic `/metrics` endpoint (wired via the orchestrator's
optional-capability type assertion) now works for Meta campaigns (LFXV2-2993).

The original wording said this "mirrors the Google Ads GA-5 pattern exactly" and that the endpoint
now works for Meta "same as Google Ads". Neither was true of the tree: there is no
`GoogleAdsDispatcher.ReadMetrics` on `main`, and `docs/knowledge/code/internal-platform-googleads.md`
records Google Ads metrics reads as a later slice. GA-5 exists, but as an unmerged PR — which a
reader of this fragment has no way to know. Corrected to describe only what is actually here.
