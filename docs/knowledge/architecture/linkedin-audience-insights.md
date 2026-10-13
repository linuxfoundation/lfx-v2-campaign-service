---
type: "Architecture Doc"
title: "LinkedIn Audience Insights"
description: "The LinkedIn professional-demographic read behind the Optimize tab: job function, seniority, industry, company size and country buckets keyed by LinkedIn's verbatim URNs, read live from Ad Analytics for the project's own campaigns on its own connection, every counter flagged approximate because LinkedIn approximates and thresholds demographics."
resource: "internal/service/connection_linkedin_audience.go"
---

# LinkedIn Audience Insights

`GET /projects/{project_id}/linkedin-ads/audience?window=` (`get-linkedin-ads-audience`,
`campaign_manager`, LFXV2-2665) — the LinkedIn sibling of `/meta-ads/audience`,
`/twitter-ads/audience` and `/microsoft-ads/audience`. Design in `design/linkedin_audience.go`
(its method is declared by `linkedInAdsAudienceMethod()`, called from the connections service in
`design/connection.go`); service in `internal/service/connection_linkedin_audience.go`
(`LinkedInAudienceReader`, `Orchestrator.ReadLinkedInAudienceInsights`,
`ConnectionService.GetLinkedinAdsAudience`); dispatcher in `internal/dispatch/linkedin_audience.go`;
client in `internal/platform/linkedin/audience.go`.

## What LinkedIn documents (verified 2026-10-13)

Sources: [Reporting](https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads-reporting/ads-reporting),
[Metrics Available / query parameters](https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads-reporting/ads-reporting-schema),
[URN Resolution](https://learn.microsoft.com/en-us/linkedin/marketing/urn-resolution).

- **Pivots.** The `pivot` enum documents `MEMBER_COMPANY_SIZE`, `MEMBER_INDUSTRY`,
  `MEMBER_SENIORITY`, `MEMBER_JOB_TITLE`, `MEMBER_JOB_FUNCTION`, `MEMBER_COUNTRY_V2`,
  `MEMBER_REGION_V2`, `MEMBER_COMPANY`, `MEMBER_COUNTY` and (from 202609)
  `MEMBER_DESIGNATED_MARKET_AREA`. This read uses five: job function, seniority, industry,
  company size, country (`_V2`, the Bing-location dataset). Job title, company, region, county and
  DMA are long-tailed value spaces that the top-100 cap below truncates hardest (and DMA carries a
  Nielsen display obligation), so they are out of scope.
- **Approximate.** "To protect LinkedIn member privacy, professional demographic metrics are
  approximate." Sums across days, time ranges or reporting levels can disagree.
- **Thresholded.** Values with fewer than 3 events are dropped; values are not returned for ads
  reaching too few members; only the top 100 values per creative per day are kept.
- **Delay.** Demographic metrics can be delayed 12–24 hours (performance metrics are near
  real-time).
- **Retention.** Demographics are retained two years (performance data ten); every window here is
  inside it.
- **Metrics.** `impressions`, `clicks` and `costInUsd` are available with `MEMBER_` pivots (the
  "non-demographic pivots only" list is `approximateMemberReach`, card and viral-card metrics and
  `conversionValueInLocalCurrency`); cost "is not adjusted for over delivery when a member
  professional demographic pivot is specified".
- **Shape.** No pagination; responses are limited to 15,000 elements. The statistics finder takes
  up to three pivots. `campaigns` is a sufficient facet on its own ("Yes unless another facet is
  provided").
- **Values.** `pivotValues` are URNs (`urn:li:function:{id}`, `urn:li:seniority:{id}`,
  `urn:li:industry:{id}`, `urn:li:geo:{id}`), resolved to names by separate Standardized Data
  calls (`/functions`, `/seniorities`, `/industries`, `/geo`). The company-size value format is not
  documented on the reporting pages, so it is passed through under the charset only.

## Behaviour

- **Values are URNs, not names.** Resolving would cost up to four extra Standardized Data calls
  per read, against the same 20s budget, for display text the UI can resolve or cache itself; a
  name this service invented would be worse than none. The design says so on `value`.
- **`approximate` is always `true`** — a property of the source, not of an answer, so it is set
  even on an empty result. A dimension's total sits below the campaigns' own totals; it must not
  be reconciled against the LinkedIn campaign metrics read
  (`GET .../briefs/{brief_id}/campaigns/{campaign_id}/metrics`).
- **Request.** Per pivot, one `GET /adAnalytics?q=statistics&pivots=List(CAMPAIGN,MEMBER_…)
  &timeGranularity=ALL&dateRange=(start:…,end:…)&campaigns=List(<campaign URNs>)
  &accounts=List(urn:li:sponsoredAccount:<bound account>)&fields=impressions,clicks,costInUsd,pivotValues`,
  sequential, single attempt (a 429 is a 503). The CAMPAIGN pivot is there so every returned
  element can be proven to belong to the project; with a single MEMBER_ pivot the rows would carry
  no evidence the facet was honoured. The `accounts` facet is the bound account, the same pair
  `GetCampaignMetrics` sends: LinkedIn campaign ids are unique only within an ad account, the token
  can reach several, and unknown provenance proceeds, so the campaign facet alone could match a
  same-id campaign on another accessible account. `pivotValues` is projected explicitly (it is in
  LinkedIn's documented fields list, and every element is required to carry it).
- **Window.** `today`, `last_7_days`, `last_30_days` (default), `this_month`, `last_month` — the
  LinkedIn metrics read's UTC `dateRange` mapping (`dateRangeForWindow`). `yesterday` and
  `last_14_days` are 400 there, so they are 400 here, refused by the design enum and by
  `resolveLinkedInAudienceWindow`. `today` is served but is mostly empty given the 12–24h delay.
- **Counters.** An absent counter reads 0 (the schema's default for an unreported long); an
  explicit `null` is NOT a 0 and fails the read, as do non-integer, negative or overflowing
  values, clicks reported with zero impressions (an incomplete element, the metrics read's guard), and a non-string or malformed `costInUsd`. Cost is USD micros, the unit the LinkedIn
  metrics read already publishes; no currency field.
- **All or nothing.** Any pivot failing, a foreign or duplicated element, a wrong URN type, an
  out-of-charset value, a `next` paging link or a response at the 15,000 ceiling fails the whole
  read (503, fixed text, no upstream content). `identityjson.Check` runs on the raw bytes first.

## Trust boundary

- **Own connection only.** `resolveLinkedInCredentials(..., d.creds.resolveOwned)` — no LF system
  fallback, like the LinkedIn account monitor and the Microsoft audience read. A project with no
  LinkedIn connection of its own is 404. (Meta's audience read resolves through
  `resolveExisting`, which does allow the fallback; this read deliberately follows the stricter
  monitor/Microsoft precedent because LinkedIn genuinely runs several ad accounts across
  foundations.)
- **Bound account only.** Every scope entry's recorded creation account
  (`linkedInCreationAccountID`: the stamped `accountId`, else the `linkedInUrl` segment) must equal
  the connection's account; ANY mismatch, mixed scope included, is 409
  (`ErrCampaignAccountMismatch`) before LinkedIn is contacted. Unknown provenance proceeds.
- **Project campaign scope.** The scope is this service's own rows (`projectCampaignScope`); an
  empty scope answers `buckets: []` without contacting LinkedIn. A non-canonical stored id or more
  than 50 distinct campaigns (a local URL bound) is 409 (`ErrAudienceScopeInvalid` /
  `ErrAudienceScopeTooLarge`), checked before credentials are decrypted, as is the window.
- **Errors.** `classifyInsightsErrorFor` with the `linkedInAdsAudienceInsights` descriptor:
  inactive / incomplete / unselected account / expired token (401, re-tagged by `linkedinExpiry`)
  are 400; everything unverifiable is 503 with the shared fixed text.
- **No gate.** LinkedIn has no metrics feature flag (`LINKEDIN_*_ENABLED` does not exist; its
  metrics read and account monitor are ungated), so this read is not gated either.

## Routing

`charts/lfx-v2-campaign-service/templates/httproute.yaml` routes
`(linkedin-ads|meta-ads|twitter-ads)/audience`; `ruleset.yaml` rules
`/projects/:projectId/linkedin-ads/audience` (campaign_manager); `parity_test.go` accepts the base
path and rejects `/audience/x` and `/audiences`.

## Verification status

Verified only against LinkedIn's published documentation, not a live account. `pivotValues` is
named in `fields` as LinkedIn's documented Analytics-finder fields list shows (the account
monitor's older 400 was for the legacy singular `pivotValue`); if LinkedIn ever omits it, every
element fails the two-value check and the read is a 503, never a silent mis-attribution.
