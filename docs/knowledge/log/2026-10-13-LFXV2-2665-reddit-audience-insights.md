# 2026-10-13 — LFXV2-2665 Reddit audience insights read

**Update** — New `campaign_manager` read `get-reddit-ads-audience`,
`GET /projects/{projectId}/reddit-ads/audience`, the Reddit sibling of the Meta, X and Microsoft
audience reads. See [Reddit Audience Insights](../architecture/audience-insights-reddit.md).

- **Upstream.** Reddit Ads API v3 `POST /ad_accounts/{id}/reports`, one report per owned campaign
  per breakdown (`filter: campaign:id==<id>`, `breakdowns: [GENDER|COUNTRY|COMMUNITY]`, the
  dimension also requested as a field). Dimensions verified against Reddit's published OpenAPI
  document: GENDER, COUNTRY and COMMUNITY are in both the `breakdowns` and `fields` enums; **AGE is
  not a breakdown**, so there is no age dimension. New `internal/platform/reddit/audience.go`.
- **Bounds.** At most 8 campaigns (24 reports) → 409 `ErrAudienceScopeTooLarge`; 3 reports in
  flight; 10 pages per report.
- **Trust.** Own connection only (`resolveOwned`; no LF fallback → 404), bound account only, any
  campaign created under another account → 409, invalid stored id → 409. `identityjson.Check` on
  each raw body; a missing or explicit-null counter, foreign row, missing breakdown key,
  out-of-charset or repeated value fails the whole read (503, fixed text, no partial buckets). An
  explicit null breakdown VALUE is a documented answer and becomes a bucket with no `value`.
- **Gate.** `REDDIT_METRICS_ENABLED` (default off), checked before the scope lookup.
- **Plumbing.** `service.RedditAudienceReader`, `Orchestrator.ReadRedditAudienceInsights`,
  `model.RedditAudienceInsights`/`RedditAudienceBucket`, `ConnectionService.GetRedditAdsAudience`
  with its own window subset (no `yesterday`/`last_14_days`).
- Chart: HTTPRoute regex `(meta-ads|reddit-ads|twitter-ads)/audience`, RuleSet entry and parity
  rows. API catalog row added; the Meta and X rows no longer say Reddit has no audience route.
- Verified only against Reddit's published schema, not a live account.
- **PR #306 review.** A scope row that records no creation account is now 409 (provenance
  unknown, re-dispatch) instead of being read; every reporting request attempt — each page and each
  429 retry, from this read, the metrics read and the account monitor — takes a slot in a
  per-account, per-process budget (45 per 60 s, 24 for this read) or the read fails whole (503);
  identical concurrent reads share one upstream read and successes are reused for 60 s; the
  cross-campaign overflow refusal is now tested.
- **Kubernetes concepts.** `kubernetes/httproute.md` and `kubernetes/ruleset.md` now list
  `(meta-ads|reddit-ads|twitter-ads)/audience`, and `kubernetes/deployment.md` says
  `REDDIT_METRICS_ENABLED` also gates the audience read, not only `ReadMetrics`.
