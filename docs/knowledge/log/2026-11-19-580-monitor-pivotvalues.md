# 2026-11-19 — #580 LinkedIn account monitor projects pivotValues

**Fix** — The LinkedIn account monitor's Ad Analytics read (`monitor.go`, `pivot=CAMPAIGN`) now
names `pivotValues` in `fields`. It used to rely on LinkedIn sending `pivotValues` with a pivot
read even when unprojected. At `LinkedIn-Version: 202611` (#639, released in v1.0.38) the finder
returns only projected fields, so in prod every element arrived without `pivotValues` and every
LinkedIn monitor read failed with `decode account monitor analytics: element has no pivotValues`
(Datadog, all three prod pods, 18 Nov 2026 22:48–22:49 UTC). Self-serve showed it as a 503.

The audience read already projected `pivotValues`, and the single-campaign metrics read does not
need it, so neither is affected. `TestListAccountCampaigns_ProjectsPivotValues` pins the field in
the request. This is the first live finding of the #580 202611 probe.
