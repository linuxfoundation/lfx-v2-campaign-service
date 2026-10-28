# 2026-10-28 — geo search records only calls that reached Google (PR #411)

**Fix** — `GeoTargetSuggester.SuggestGeoTargets` returned `cached`, and the orchestrator recorded
an upstream call whenever it was false. That included a missing or unusable connection refused
locally before any request was sent. It now returns `called`, which is true only when Google was
contacted, whether that call succeeded or failed. The `suggest_geo_targets` metric is recorded
only on that path, so cache hits and local refusals are not counted.
