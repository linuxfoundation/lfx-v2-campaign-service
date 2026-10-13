---
type: "Architecture Doc"
title: "Reddit Audience Insights"
description: "The Reddit sibling of the Meta, X and Microsoft audience reads: gender, country and community buckets from Reddit Ads API v3 report breakdowns (no age — Reddit has no AGE breakdown), scoped to the project's own campaigns on its own connection and bound account, one filtered report per campaign per breakdown, gated by REDDIT_METRICS_ENABLED and failing closed."
resource: "internal/platform/reddit/audience.go"
---

# Reddit Audience Insights

`GET /projects/{project_id}/reddit-ads/audience?window=` (`get-reddit-ads-audience`,
LFXV2-2665), a `campaign_manager` read. The full contract (statuses, bounds, field meanings) is
the `/reddit-ads/audience` row of [the API catalog](../../api-catalog.md); this concept records
what the read is built on and why it is shaped the way it is.

## What Reddit offers

Source: Reddit's published Ads API v3 OpenAPI document (`https://ads-api.reddit.com/api/v3/openapi.json`,
the "Download Specs" link of `https://ads-api.reddit.com/docs/v3/`), operation
`POST /ad_accounts/{ad_account_id}/reports`. `ads-api.reddit.com` refuses automated fetches from
the authoring tooling (the LFXV2-3282 log entry records the same), so the document was read from a
byte-for-byte copy pinned with a SHA-256 checksum in a public repository
(`goncaloaguer/unofficial-reddit-ads-mcp`, `openapi/reddit-ads-v3.json`); its `info` and `servers`
blocks are Reddit's own.

- The request's `breakdowns` enum is `AD_ACCOUNT_ID, AD_GROUP_ID, AD_ID, CAMPAIGN_ID, COUNTRY,
  DATE, HOUR, DMA, METRO, CAROUSEL_CARD, GALLERY_ITEM_ID, GENDER, INTEREST, KEYWORD, PLACEMENT,
  OS_TYPE, ASSET_ID, REGION, COMMUNITY, LANGUAGE` — up to 3 per report (4 with COUNTRY and REGION).
- **There is no AGE breakdown.** `AGE` is in the `fields` enum and `age` is a response property,
  and one of the spec's own examples lists `AGE` among `breakdowns` — but the enum, which is what
  the request schema validates against (`additionalProperties: false`), does not include it. An age
  dimension would rest on the example over the schema, so it is not built.
- The response rows (`Metric`) carry `gender`, `country`, `community` typed `["string","null"]`,
  and `impressions`, `clicks`, `spend` typed `["integer","null"]` (`spend` in microcurrency).
- The reporting rate-limit policy (`ads-reporting`) is **60 requests per 60 seconds**.

## Choices

- **Dimensions: GENDER, COUNTRY, COMMUNITY.** Each is in both the `breakdowns` and `fields` enums,
  so it is requested as both — grouping by it and echoing it onto the row, as `CAMPAIGN_ID` is
  requested as a field for provenance. Community (the subreddit) is Reddit's own audience
  dimension; gender is the one demographic Reddit breaks reports down by. PLACEMENT, OS_TYPE,
  INTEREST, REGION, DMA are documented too and left for later: each extra dimension costs one more
  request per campaign against the 60-per-minute quota.
- **One report per campaign per breakdown.** The only `filter` form this repository has verified
  is the single term `campaign:id==<id>` (the campaign metrics read and the account monitor). The
  spec says `filter` is "a comma-separated list" but not whether two terms on the same field are
  OR-ed or AND-ed; an AND would answer an empty report — read as no delivery — for a project that
  had plenty. Per-campaign requests trade request count for a scope this read can prove, and every
  row is still checked against the requested campaign.
- **At most 8 campaigns** (`reddit.MaxReportCampaigns`), so a read is at most 24 requests — under
  half the reporting quota, which the metrics read and account monitor share. More is
  `ErrAudienceScopeTooLarge` (409), refused before any request. At most 3 reports in flight, each
  walking at most 10 pages, inside the orchestrator's 20s `metricsCallTimeout`.
- **Own connection only, bound account only** (`resolveOwned`, as the account monitor): never the
  LF system fallback, and the client reads only the connection's own `account_id`. Any campaign in
  scope whose recorded creation account differs is `ErrCampaignAccountMismatch` (409) for the whole
  read; a stored id that is not a valid Reddit campaign id is `ErrAudienceScopeInvalid` (409).
- **Fail closed.** `identityjson.Check` on each raw response body before decoding (a repeated or
  case-folded key anywhere fails the read); a missing **or explicit `null`** counter is refused,
  never read as 0 (the campaign metrics read's `reportRow.validate`); a missing breakdown key, an
  out-of-charset value, a repeated value, a foreign row, a negative counter, clicks without
  impressions, overflow or an unfinished pagination walk fails the whole read — 503 with fixed
  text, no partial buckets, no upstream text.
- **An explicit `null` breakdown value is a bucket, not an error.** The spec types every breakdown
  value as nullable, so null is a documented answer — traffic Reddit did not attribute. It is
  returned with no `value`, so each dimension still totals its traffic. An ABSENT key is different
  (the report was not grouped by the breakdown) and fails the read.
- **Gated by `REDDIT_METRICS_ENABLED`**, default off, like the metrics read on the same operation:
  matched to the published schema, never exercised against a live ad account. Checked before the
  scope lookup, so a disabled read is 400 even for a project with no Reddit campaigns.

## Layers

| Layer | Piece |
| --- | --- |
| Design | `design/connection.go` `get-reddit-ads-audience`; `design/brief.go` `RedditAdsAudience`, `RedditAdsAudienceBucket`, `redditAudienceWindowEnum` |
| Service | `internal/service/connection_reddit_audience.go` (`GetRedditAdsAudience`, window subset, `classifyInsightsErrorFor`); `Orchestrator.ReadRedditAudienceInsights` + `RedditAudienceReader` in `orchestrator.go` |
| Dispatch | `internal/dispatch/reddit_audience.go` (`AudienceEnabled`, `ReadRedditAudienceInsights`, `redditScopeForAccount`) |
| Platform | `internal/platform/reddit/audience.go` (`GetAudienceInsights`) on the shared broken-down reader `report_breakdown.go`, which the [keyword performance read](keyword-performance-reddit.md) also uses |
| Chart | `httproute.yaml` `(meta-ads|reddit-ads|twitter-ads)/audience`; `ruleset.yaml` entry; `parity_test.go` rows |

## Not verified

No request has been made against a live Reddit ad account. Unknown until one is: whether a
breakdown value is ever actually `null`, the exact gender vocabulary, whether a campaign with no
delivery yields an empty `metrics` array or zero rows per value, and whether `ends_at` includes its
final hour (the metrics read records the same caveat).
