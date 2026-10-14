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
- **Finders.** `adAnalytics` has an Analytics finder (`q=analytics`, ONE `pivot`) and a
  Statistics finder (`q=statistics`, up to three `pivots`). The MEMBER_* values appear ONLY in the
  Analytics finder's `pivot` enum; the Statistics finder's `pivots` enum lists none of them
  (ads-reporting-schema, "Analytics Finder Query Parameters" vs "Statistics Finder Query
  Parameters"). So a demographic pivot cannot be paired with CAMPAIGN in one request. The Analytics
  finder requires `q`, `dateRange.start`, `timeGranularity`, `fields` and at least one facet
  (`campaigns`, `accounts`, … — "Yes unless another facet is provided"); requests carry the
  `Linkedin-Version: YYYYMM` and `X-Restli-Protocol-Version: 2.0.0` headers (both already sent by
  this client), and the documented samples use the record syntax
  `dateRange=(start:(year:…,month:…,day:…),end:(…))`, `timeGranularity=ALL`, `pivot=MEMBER_…`.
- **Shape.** No pagination; responses are limited to 15,000 elements. An Analytics element on a
  MEMBER_ pivot carries only that pivot's value in `pivotValues` (the `MEMBER_COMPANY` sample:
  `["urn:li:organization:1111"]`), never the campaign.
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
- **Request.** One `GET /adAnalytics?q=analytics&pivot=MEMBER_…&timeGranularity=ALL
  &dateRange=(start:(day:…,month:…,year:…),end:(…))&campaigns=List(urn:li:sponsoredCampaign:<ONE id>)
  &accounts=List(urn:li:sponsoredAccount:<bound account>)&fields=impressions,clicks,costInUsd,pivotValues`
  per (pivot, campaign) — at most 5 × 10 = 50 requests, 5 in flight at a time
  (`audienceConcurrency`), single attempt each (a 429 is a 503; the first failure cancels the
  rest), all inside the orchestrator's 20s `metricsCallTimeout`. The per-campaign answers are summed
  per value. Campaign attribution comes from the REQUEST: because the Analytics finder takes one
  pivot and its elements carry no campaign, each request names exactly one campaign. The `accounts`
  facet is the bound account, the same pair `GetCampaignMetrics` sends: LinkedIn campaign ids are
  unique only within an ad account, the token can reach several, and unknown provenance proceeds,
  so the campaign facet alone could match a same-id campaign on another accessible account.
  `pivotValues` is projected explicitly, and this was re-checked after review (#322): the
  ads-reporting page's "Requesting specific metrics in the Analytics Finder" lists `pivotValues`
  among the supported `fields`, its `pivot=MEMBER_COMPANY` sample sends
  `fields=…,costInLocalCurrency,pivotValues`, its `MEMBER_COUNTRY_V2` and URN-resolution samples
  send `fields=impressions,pivotValues`, and it states that without `fields` "only impressions and
  clicks are returned by default". No page says `pivotValues` is rejected or returned
  automatically. The 400 recorded in `monitor.go` names the legacy SINGULAR `pivotValue`, which is
  not a schema field; that comment now says so. Every element is required to carry exactly one
  value, so omitting the projection would risk a 503 on every read.
- **Cancellation.** The first failing request cancels the read's context: requests in flight are
  cancelled, tasks still queued behind the limiter check the context and never reach LinkedIn,
  and no partial result is returned; the error is the failing request's own (`*apiError`, 500 in
  the test), never a sibling's cancellation. `TestGetAudienceInsights_FirstFailureCancelsInFlightAndQueued`
  pins all three with two campaigns (10 requests, 5 queued).
- **What the response can and cannot prove.** Checked: every element carries exactly ONE
  `pivotValue`, of the requested pivot's URN type, and never a `urn:li:sponsoredCampaign` (a CAMPAIGN
  grouping in the answer means LinkedIn did not apply the requested pivot). NOT checkable: that
  LinkedIn honoured the `campaigns` and `accounts` facets — an Analytics element names no campaign
  or account, so that rests on the facets, as it does for the LinkedIn campaign metrics read.
  Summing campaign-level demographics is itself an approximation LinkedIn warns about ("adding
  campaign level data and comparing it to account level data"), which `approximate` already covers.
- **Window.** `today`, `last_7_days`, `last_30_days` (default), `this_month`, `last_month` — the
  LinkedIn metrics read's UTC `dateRange` mapping (`dateRangeForWindow`). `yesterday` and
  `last_14_days` are 400 there, so they are 400 here, refused by the design enum and by
  `resolveLinkedInAudienceWindow`. `today` is served but is mostly empty given the 12–24h delay.
- **Counters.** An absent counter reads 0 (the schema's default for an unreported long); an
  explicit `null` is NOT a 0 and fails the read, as do non-integer, negative or overflowing
  values, clicks reported with zero impressions (an incomplete element, the metrics read's guard), and a non-string or malformed `costInUsd`. Cost is USD micros, the unit the LinkedIn
  metrics read already publishes; no currency field.
- **All or nothing.** Any request failing, a duplicated element, a wrong URN type, an
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
  than 10 distinct campaigns (a local bound on request count: 50 requests per read) is 409 (`ErrAudienceScopeInvalid` /
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

Verified only against LinkedIn's published documentation, not a live account. The first version
(#314) paired CAMPAIGN with each MEMBER_* pivot on the Statistics finder, which LinkedIn's
documented `pivots` enum does not allow, so every non-empty read would have been rejected (503);
it now uses the Analytics finder per campaign. `pivotValues` is named in `fields` as LinkedIn's
documented Analytics-finder fields list shows (the account monitor's older 400 was for the legacy
singular `pivotValue`); if LinkedIn ever omits it, every element fails the one-value check and the
read is a 503, never a silent mis-attribution.
