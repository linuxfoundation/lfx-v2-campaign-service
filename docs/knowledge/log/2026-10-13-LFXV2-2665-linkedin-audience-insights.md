# 2026-10-13 — LFXV2-2665 LinkedIn audience insights

**Update** — Added `get-linkedin-ads-audience` (`GET /projects/{project_id}/linkedin-ads/audience?window=`,
`campaign_manager`): LinkedIn professional demographics — `job_function`, `seniority`, `industry`,
`company_size`, `country` (Ad Analytics `MEMBER_JOB_FUNCTION`, `MEMBER_SENIORITY`,
`MEMBER_INDUSTRY`, `MEMBER_COMPANY_SIZE`, `MEMBER_COUNTRY_V2`) — across the project's own campaigns,
read live with one `q=statistics&pivots=List(CAMPAIGN,MEMBER_…)` request per pivot so every element
is proven to belong to an in-scope campaign. Values are LinkedIn's URNs verbatim (not resolved to
names); `approximate` is always true because LinkedIn documents demographics as approximate,
thresholded (under 3 events dropped, top 100 per creative per day) and delayed 12–24h. Own
connection only (no LF fallback, 404), bound account only (409 on any provenance mismatch), at most
50 campaigns (409), the LinkedIn metrics read's five windows (`yesterday`/`last_14_days` 400),
`identityjson` on raw bytes, explicit `null` counters refused rather than read as 0, 503 with fixed
text otherwise. Not gated (LinkedIn has no metrics flag). Chart route regex, RuleSet and parity
table admit the path; the api-catalog's "Reddit, LinkedIn: no audience route" wording now names
Reddit only. New concept
[LinkedIn Audience Insights](../architecture/linkedin-audience-insights.md); audience section added to
[internal/platform/linkedin](../code/internal-platform-linkedin.md).

**Fix** — PR #314 review: each statistics request now also carries an `accounts` facet of the
connection's bound sponsored account (LinkedIn campaign ids are unique only within an ad account,
one token reaches several, and unknown provenance proceeds, so the campaign facet alone could
match a same-id campaign elsewhere); `GetAudienceInsights` takes the account id and validates it
first. `pivotValues` is now projected explicitly in `fields`, as LinkedIn's documented fields list
shows, rather than relied on to arrive unrequested. An element reporting clicks with zero
impressions is refused as incomplete (the LinkedIn metrics read's guard). The docs no longer name
a nonexistent `/linkedin-ads/metrics` route; they point at the campaign metrics read.
