# 2026-10-13 — Account monitor: unmeasured rows counted, failures classified and logged (LFXV2-2665)

**Update** — `AccountMonitorTotals` gains required `unmeasured_campaigns` (≥ 0): the number of
`fetch_failed` rows in the sum, an upper bound on the campaigns whose delivery the totals may be
missing (#218). Each non-Google platform client now classifies a failed row into a closed
`model.MonitorFailureClass` (`auth`, `throttle`, `upstream_5xx`, `decode`, `no_report`, `other`;
Google rows arrive unclassified and count as `other`), and the service logs ONE warning per
monitor request with per-class counts (project id and platform, no account id, no upstream text;
none when every failure is `no_report`) and adds them to the new
`campaign_monitor_unmeasured_rows_total{platform,class}` counter (#220). Reddit's fan-out now fails
the read when the caller's deadline killed in-flight reports. The class is not in the response.
See `architecture/account-monitor-endpoints.md` and `code/internal-infrastructure-metrics.md`.

PR review (#319): a 2xx report whose body is not valid JSON was classed `other`, because
`request()` wraps that unmarshal failure in a `transportError` before `decodeReportRows` runs. It
now carries `errUndecodable2xxResponse` inside the same `transportError` (still ambiguous for
mutating callers, same message text) and classes as `decode`; a 2xx body that could not be read
stays `other`.

PR review (#319, published example): `unmeasured_campaigns` carried `Example(2)`, so every
generated account-monitor response example reported 2 unmeasured campaigns beside campaign rows
that all read `fetch_failed: false`. The example is now 0, and
`TestPublishedAccountMonitorTotalsExamplesArePossible` checks each response example's totals
against the campaigns array beside them — `unmeasured_campaigns` must equal the number of
`fetch_failed=true` rows — rather than only bounding it by `campaign_count`.

Follow-up (#319, `campaign_count` example): `campaign_count` carried `Example(14)` beside
generated campaigns arrays of 2–4 rows (Goa picked a different array length per method). The
`campaigns` attribute of `AccountMonitor` now has an explicit two-row example: neither row is
`fetch_failed`, and together they sum to the totals example's spend, impressions, clicks and
conversions. `campaign_count` is now `Example(2)`, and the same test also requires
`campaign_count` to equal `len(campaigns)` wherever both appear.

Follow-up (#319, example pacing): the service could not have produced the two-row example. Over
`days: 30`, `rules.EvaluateGoogleMonitor` gives the first row 11% (482.13 / (150 × 30)), not 42%,
and the second row, with `budget_day: 0`, `pacing_unknown=true` and `pacing_pct` 0, not 87/normal.
The rows now spend 1890 of 150/day (42%, underspending) and 4200 of 200/day (70%, normal on the
50/90/100 ladder), and the totals spend is their sum, 6090. The composed `action_items` repeated
one finding two to four times, so `action_items` now has an explicit example: the single item the
evaluator emits for these rows, in the evaluator's own wording, naming the underspending row. The
`AccountMonitorCampaign` and action-item attribute examples were aligned to the same row.
`TestPublishedAccountMonitorTotalsExamplesArePossible` now recomputes every example row with
`EvaluateGoogleMonitor` (which labels through `StandardLadder`), requires `pacing_pct`,
`pacing_label` and `pacing_unknown` to match, and requires `action_items` to equal the
evaluator's findings. A row that is not Google-shaped fails the test instead of going unchecked.

Follow-up (#319, per-platform examples): the one `AccountMonitor` example was Google-shaped
(`campaign_url`, `is_search_channel: true`, `ENABLED`, and flight dates Google never reports) and
was reused by all six monitor operations, so five of them advertised responses their handlers
cannot return; the example test then chose the Google evaluator because it saw `campaign_url`, not
because of the endpoint. Each monitor method now has its own 200 body type, which `Extend`s
`AccountMonitor` (the wire tags are unchanged and the service still returns the shared result).
Each body type carries an example in its platform's shape, with that platform's status literal and
fields, and pacing and findings from its own evaluator; Microsoft and X also get their report-window
fields. `AccountMonitor` and `AccountMonitorCampaign` no longer carry a whole example. The new
`TestPublishedAccountMonitorExamplesMatchTheirOperation` maps each operation id to its evaluator and
fails on an unmapped one or on an account-monitor example not attached to an operation. Against the
old examples it failed on every non-Google operation, on Google's flight dates, and on the shared
OpenAPI 2 definition. See `architecture/account-monitor-endpoints.md`.

Follow-up (#319, schema property examples, metric docs, class drift):
- **Property examples.** The per-platform body types used `Extend(AccountMonitor)`, so they
  inherited `AccountMonitor`'s property examples. Every schema therefore showed `account_id`
  "8666746580", which is impossible on Meta (`act_<digits>`), and showed report-window examples on
  the four live platforms. The body types now take the fields by `Reference`. `account_id` carries
  each platform's own id, and the four report-window fields carry examples only on Microsoft and
  X; the shared type carries none. Wire fields are unchanged.
- **Schema guard.** The new `TestPublishedAccountMonitorSchemaExamplesMatchTheirPlatform` checks
  all four specs, OpenAPI 2 included. Against the old specs it reported Meta's id and the live
  platforms' report-window examples.
- **README.** `campaign_monitor_unmeasured_rows_total{platform,class}` is now in README.md's
  service-metrics table, with its bounded class set.
- **Class drift.** `NormalizeMonitorFailureClass` still maps an empty class (an unclassified row)
  to `other`. A non-empty class outside the set now maps to the new `MonitorFailureUnknown`
  ("unknown") instead of hiding in `other`. It is logged in `unmeasured_by_class` and recorded
  under the metric's `unknown` class, so the label set stays bounded. The existing per-request
  test failed against the old normalisation (`other` 2, no `unknown`).
