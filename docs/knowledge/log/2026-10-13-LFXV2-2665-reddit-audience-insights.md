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
- **PR #306 review, second round.** (1) An unsent attempt no longer spends a reporting slot: the
  client refreshes the token and builds the request first and takes the slot immediately before
  the send, and `ReportBudgets.acquire` checks `ctx.Err()` before every take, including after each
  wait (`TestReportBudgets_UnsentAttemptsSpendNothing`). (2) A scope with both a foreign-account
  and an unrecorded campaign now returns new `domain.ErrCampaignProvenanceMixed` joined with
  `ErrCampaignProvenanceUnknown` and `ErrCampaignAccountMismatch`, and the service answers a 409
  naming both remedies instead of the mismatch text alone. (3) The per-process ledger is guarded
  in the chart: `templates/deployment.yaml` refuses `REDDIT_METRICS_ENABLED` (effective value,
  either env input or a secret) with `replicaCount` > 1 or autoscaling past one, the X guard's twin
  (`TestDeploymentRefusesRedditReportsOnMultipleReplicas`). Also: the API catalog and chart now
  list the Reddit and LinkedIn audience routes together after the merge of `main`.
- **PR #306 review, third round.** (1) The Reddit replica guard is now UNCONDITIONAL: the Reddit
  account monitor is ungated, always routed and spends the same per-process reports ledger, so
  `REDDIT_METRICS_ENABLED` did not cover all the traffic. Gating the monitor instead was rejected
  (a shipped, flag-free feature would turn into a 400). The default deploy (`replicaCount: 1`, no
  HPA, Recreate) is unaffected. The X guard's test now checks which guard refused. (2) A pre-send
  dial failure now releases its reporting slot (`ReportBudgets.reserve` returns a release func);
  any other send error keeps it. (3) The Reddit concept's chart row lists the whole routed family
  `(linkedin-ads|meta-ads|reddit-ads|twitter-ads)/audience`, as does the LinkedIn concept's.
