# 2026-08-18 — LFXV2-3313 brief-wide metrics read

**Creation** — `GET /projects/{projectId}/briefs/{briefId}/metrics` reads every campaign on a
brief in one request, so the Monitor and Optimize tabs stop fanning out one request per
campaign per platform and assembling the result themselves.

**Why an aggregate could not just reuse the campaign-scoped handler.** `GetCampaignMetrics`
distinguishes six failure modes as distinct HTTP responses — window unsupported, unprovisioned,
no data in window, provenance unknown, account mismatch, unusable connection. An aggregate
cannot express those as HTTP responses, because one campaign's 409 would fail the other five.
Each becomes a per-row status instead, collapsed only where the operator's next action is
identical: `unsupported`, `not_ready`, `connection_problem`, `failed`, and `ok`.

`not_ready` and `failed` stay separate deliberately. A staged email draft and an ad-platform
outage produce the same absence of numbers and want opposite responses — one is the expected
steady state before a human presses send, the other is an incident.

**A non-`ok` row carries no `metrics` at all rather than zeroes.** The design attribute is
optional precisely so the schema permits that. A zero is a measurement, and substituting one
for a campaign that could not be read is indistinguishable from a campaign that genuinely
served nothing — the failure-as-measurement shape that produced a false "pause losing
campaigns" recommendation on the ED dashboard. `ok_count` is reported alongside the rows so a
consumer can see a cross-campaign total covers 2 of 6 before presenting it.

**Each `g.Go` returns nil even on error.** Returning it would cancel the errgroup's context and
abandon campaigns whose reads had not yet started, so the aggregate would report failures it
never attempted. Mutation-verified: propagating the error fails four tests.

**No cross-channel cost total.** `cost_micros` is micro-units of each platform's own native
currency and this service does no FX conversion, so summing LinkedIn USD with X's billing unit
would produce a number with no currency and no meaning.

**The window is per-row.** With no request-level window the default is platform-aware — X Ads
caps queryable ranges at 7 days — so rows can legitimately cover different periods. Each row
reports the window IT was read over; the top-level value is the requested one and does not
claim to cover every row. The row's window comes from the resolved value rather than the
adapter's echo, for the reason `GetCampaignMetrics` already documents: adapters are not
required to echo it back, and trusting them emits `""` and violates the response enum.

**Repository** — `ListCampaignsForBrief` is new on `CampaignReader`. It excludes soft-deleted
rows, matching `GetCampaign`, and returns a non-nil empty slice for a brief with no campaigns:
that is what every brief looks like before it is dispatched, and the caller must be able to
answer "nothing to measure yet" without it being a failure.

**Tests** — Nine, including two live-Postgres cases. The live tests caught three things a fake
could not: `brief_id` is `UUID` (not the descriptive string `dbtest.UniqueID` returns), it
carries a foreign key to `campaign_briefs(id)` so a brief must actually exist, and
`campaign_briefs` has no `payload` column.

**Note on a test that is NOT revert-binding.** The `ORDER BY platform, variant` is asserted but
removing it does not fail the live test: the planner serves this query from
`uq_campaigns_brief_platform_variant_live`, keyed on `(brief_id, platform, variant)`, so an
index scan already returns rows in the asserted order at these row counts — confirmed with
`EXPLAIN`. The clause stays because that is an accident of the current plan: a bitmap heap scan
or parallel seq scan on a larger table returns no order, and the consumer's table would start
reshuffling between reads with nothing failing to say so. The limitation is recorded in the
test comment rather than claimed as coverage it does not have.

**Chart** — No change needed, verified rather than assumed. The HTTPRoute regex's
`(briefs|jobs|hubspot)(/.*)?` arm matches the new path, and the RuleSet already carries
`/projects/:projectId/briefs/**`. The parity test passes.

**Scope** — Aggregation only. No action items, no pacing, no keyword flags: those are rules,
they differ per platform today, and they belong in their own change once this shape is proven.
Attribution model does not arise here — `CampaignMetrics` carries impressions, clicks, cost and
CTR, with no revenue or conversions, because the ad-platform APIs do not return them.
