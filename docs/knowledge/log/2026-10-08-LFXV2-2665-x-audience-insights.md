# 2026-10-08 — LFXV2-2665 X audience insights read

**Update** — New `campaign_manager` read `get-twitter-ads-audience`,
`GET /projects/{projectId}/twitter-ads/audience`, the X sibling of `get-meta-ads-audience`.

- **Upstream (asynchronous).** X documents segmentation as available only through asynchronous
  analytics; the synchronous `GET stats/accounts/:account_id` takes no `segmentation_type`. One
  read: `GET accounts/:account_id` (timezone, currency), one paced
  `POST stats/jobs/accounts/:account_id` per segmentation (AGE, GENDER, PLATFORMS) per batch of
  ≤20 own campaign ids (`entity=CAMPAIGN`, `granularity=TOTAL`, `placement=ALL_ON_TWITTER`,
  `metric_groups=ENGAGEMENT,BILLING`), status polling with `job_ids`, then the results files
  through the monitor's `downloadStatsFile`. It waits inside `metricsCallTimeout` (20s) rather
  than persisting a report; unfinished jobs are 503 and expire on X. The monitor's job POST was
  factored into `postStatsJob` (no behaviour change). New `internal/platform/twitter/audience.go`.
- **Window.** `today`, `yesterday`, `last_7_days` (default) only — the X metrics read's windows,
  though X's segmented-job ceiling is 45 days — computed on the ACCOUNT's calendar; others 400
  with a fixed message; a non-whole-hour zone is 409 (`ErrAccountTimezoneUnsupported`, new arm in
  `classifyInsightsErrorFor`). The X metrics read itself still computes its days in UTC; not
  changed here.
- **Trust.** `identityjson.Check` on every account, job, status and file body; file rows must name
  a campaign of THEIR job's batch; repeated rows, out-of-charset segment names, malformed counters
  and int64 overflow fail the whole read (503, no partial buckets, nothing echoed). Absent/null
  counters are 0.
- **Plumbing.** `service.TwitterAudienceReader` (only `TwitterDispatcher`),
  `Orchestrator.ReadTwitterAudienceInsights` on the shared `readScopedAudience`,
  `model.TwitterAudienceInsights`/`TwitterAudienceBucket`, the existing audience-scope 409
  sentinels (bound: 40 campaigns, six jobs). Gated on `TWITTER_METRICS_ENABLED` like the X monitor
  (off → 400 not supported). X-named design types with type- and property-level examples, pinned
  by `TestPublishedTwitterAudienceExamplesArePossible`.
- Chart: HTTPRoute regex, RuleSet entry and parity rows for `twitter-ads/audience`.
- Verified only against X's published Ads API documentation and developer forum (segmentation
  is async-only, the 20-id job cap, `segment_name`), not against a live account.
