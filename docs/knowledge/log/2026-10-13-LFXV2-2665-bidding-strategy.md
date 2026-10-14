# 2026-10-13 — Automatic or manual bidding, chosen per campaign at create

**Update** — `microsoftConfig`, `metaConfig`, `redditConfig`, `twitterConfig` and
`linkedInConfig` take an optional `bidding` object: `{"strategy": "automatic" | "manual",
"bidAmount": n}`. Omitted means automatic. `manual` needs `bidAmount`, the starting max CPC in the
ad account's currency (LinkedIn: USD). It must be greater than zero, at most 1,000,000 and a whole
number of micro-units. Amounts are never rounded. A manual campaign is created in exactly the shape
`update-campaign-bid` accepts on its platform, so that lever can change its bid. Before this
change, every campaign this service created on Meta, Reddit and X bid automatically, so the lever
refused all of them.

| Platform | automatic | manual | source |
| --- | --- | --- | --- |
| Microsoft | campaign `BiddingScheme` `MaxClicks`, no ad group bid | `EnhancedCpc` + ad group `CpcBid` | MaxClicksBiddingScheme, ManualCpcBiddingScheme, EnhancedCpcBiddingScheme (learn.microsoft.com, campaign-management-service). ManualCpc on a Search campaign "will be ignored without error and the bid strategy will be set to enhanced CPC". |
| Meta | ad set `LOWEST_COST_WITHOUT_CAP`, billed `IMPRESSIONS` (unchanged) | `LOWEST_COST_WITH_BID_CAP`, billed `LINK_CLICKS`, `bid_amount` in minor units; traffic/leads only | Ad Set reference, Graph API v25.0 |
| Reddit | `BIDLESS` on campaign and ad group (unchanged) | `MANUAL_BIDDING` on both, `bid_type` `CPC`, ad group `bid_value` in micros; traffic only | Ads API v3 (not re-fetchable; a secondary source confirms the CBO matching rule) |
| X | line item `AUTO` (unchanged) | `MAX`, `pay_by` `LINK_CLICK`, `bid_amount_local_micro` | X Ads API v12 line items |
| LinkedIn | `MAX_CONVERSION`, `costType` `CPM` | `costType` `CPC`, `optimizationTargetType` `NONE`, `unitCost` (whole cents) | "Create and Manage Campaigns", Optimization of Campaigns (li-lms-2026-09) |

**Fix (LinkedIn)**: the create sent `costType` `CPC` and no `optimizationTargetType`. LinkedIn
defaults that to manual bidding at `unitCost` 0, under which "the campaign does not deliver". The
create now always sends one of the two shapes above. Note that LinkedIn will not activate a
`MAX_CONVERSION` campaign until a conversion is associated with it, and this service associates
none.

Microsoft's legacy `cpcBid` without `bidding` still means a manual bid at that amount, and is
recorded as manual. With `automatic`, `cpcBid` is refused. With `manual`, it must equal
`bidAmount`. A Microsoft campaign created with no bid used to get Microsoft's EnhancedCpc default;
it now gets MaxClicks.

A bad `bidding` is a synchronous `400 invalid campaign config: <configKey>.bidding: ...`. The
check runs in each lane dispatcher's new `PreflightCreate` (Reddit's existing one gains it) and
again in `Dispatch`, before any upstream call. `googleAdsConfig.bidding` is refused 400 at the
service layer; no Google code changed. Migration `000047` adds `campaigns.bidding_strategy`. A
manual create also writes its starting bid to `max_cpc_bid`. The upsert fills both columns only
while they are empty. The campaign resource now returns `bidding_strategy` and `max_cpc_bid`.
`000043`–`000046` belong to the open `fix/LFXV2-2665-adoption-binding-indexes` PR. The gap is
recorded in `allowedVersionGaps`, and whichever PR merges second may need to renumber.
