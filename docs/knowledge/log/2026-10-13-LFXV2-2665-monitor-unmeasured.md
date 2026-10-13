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
