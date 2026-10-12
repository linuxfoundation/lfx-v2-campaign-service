# 2026-10-07 — LFXV2-2665 HubSpot monitor keeps metrics_as_of's sub-seconds

**Fix** — A late PR #290 review thread on `monitor-hubspot-account`.

`metrics_as_of` was formatted with `time.RFC3339`, which drops sub-second precision, so a last
response at `14:30:00.900` was published as `14:30:00Z` — an instant EARLIER than the last read,
breaking the documented upper-bound guarantee. It is now formatted with `time.RFC3339Nano`. The
service totals test's as-of fixture carries 900ms and asserts `2026-10-08T14:30:00.9Z`.
