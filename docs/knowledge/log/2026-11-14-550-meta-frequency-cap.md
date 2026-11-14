# 2026-11-14 — #550 Meta frequency cap on awareness ad sets

**Creation** — Gap 18 of the Meta gap review (#550). An awareness ad set had no frequency control.
`metaConfig.frequencyCap` (`maxImpressions` per person every `intervalDays`, 1–90 each) is sent as
the ad set's `frequency_control_specs`. Meta takes a cap only on a REACH ad set, so it is refused on
every objective but awareness rather than refused by Meta after the campaign exists. Omitted, the
payload is unchanged.

Behind `META_FREQUENCY_CAP_ENABLED` (default off) until a live probe. Every refusal is a synchronous
400 and is re-checked in the job before any request. `PreflightCreate` decodes the object on its own
so a wrong type is a 400; that decode and `decodeMetaTargeting` now share `decodeMetaConfigObject`,
with the targeting messages unchanged. The cap is checked from that strict decode, not from the
best-effort one, which leaves a zero cap behind a wrong-typed value and would report a misleading
range error.

Tests: `delivery_options_test.go` (the spec on the ad set, none without a cap, seven refusals with
no request) and `meta_optimization_goal_test.go` (flag off and on, a wrong type beside a broken
budget, two case-variant keys, Dispatch's wiring and refusal). Thirteen hand mutations each fail a
test.
