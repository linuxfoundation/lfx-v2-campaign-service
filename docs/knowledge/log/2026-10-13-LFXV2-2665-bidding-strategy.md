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
| Microsoft | campaign `BiddingScheme` `MaxClicks`, no ad group bid | `EnhancedCpc` + ad group `CpcBid` | MaxClicksBiddingScheme, ManualCpcBiddingScheme, EnhancedCpcBiddingScheme (learn.microsoft.com, campaign-management-service); "Budget and Bid Strategies" guide. ManualCpc on a Search campaign "will be ignored without error and the bid strategy will be set to enhanced CPC". |
| Meta | ad set `LOWEST_COST_WITHOUT_CAP`, billed `IMPRESSIONS` (unchanged) | `LOWEST_COST_WITH_BID_CAP`, billed `LINK_CLICKS`, `bid_amount` in minor units; traffic/leads only | Ad Set reference, Graph API v25.0 |
| Reddit | `BIDLESS` on campaign and ad group (unchanged) | `MANUAL_BIDDING` on both, `bid_type` `CPC`, ad group `bid_value` in micros; traffic only | Ads API v3 (not re-fetchable; a secondary source confirms the CBO matching rule) |
| X | line item `AUTO` (unchanged) | `MAX`, `pay_by` `LINK_CLICK`, `bid_amount_local_micro` | X Ads API v12 line items |
| LinkedIn | `objectiveType` `WEBSITE_VISIT`, `MAX_CLICK`, `costType` `CPM`, no `unitCost` | `objectiveType` `WEBSITE_CONVERSION`, `costType` `CPC`, `NONE`, `unitCost` (whole cents) | "Ad Budget Type Allowable Combinations" and "Create and Manage Campaigns" (li-lms-2026-09) |

**Fix (LinkedIn)**: the create sent `WEBSITE_CONVERSION`, `costType` `CPC` and no
`optimizationTargetType`. LinkedIn defaults that to manual bidding at `unitCost` 0, under which "the
campaign does not deliver". The create now sends one of the two explicit shapes above.

**Decision (2026-10-13, Misha)**: automatic LinkedIn campaigns optimise for clicks via
`WEBSITE_VISIT`/`MAX_CLICK`, because `MAX_CONVERSION` needs a conversion association this service
doesn't create. In "Ad Budget Type Allowable Combinations", the Objective Type Descriptions row
`WEBSITE_CONVERSION` allows only `NONE`, `ENHANCED_CONVERSION` and `MAX_CONVERSION`. Row
`WEBSITE_VISIT` allows `NONE`, `MAX_CLICK`, `ENHANCED_CONVERSION`, `TARGET_COST_PER_CLICK` and
`CAP_COST_AND_MAXIMIZE_CLICKS`. In "Create and Manage Campaigns", the Website Traffic row
`STANDARD_UPDATE | AUTO | MAX_CLICK` gives `costType` `CPM` (Bid Type `CPC`) and requires no
conversion tracking, so automatic sends `CPM`. The same page says a `MAX_CONVERSION` campaign "must
have a conversion associated with it to be activated". So the objective follows the strategy:
manual keeps `WEBSITE_CONVERSION`.

Knock-on changes:
- The campaign name's Objective segment is `Traffic` for automatic and `Conversions` for manual,
  so automatic campaigns get a new `utm_term`.
- Reuse by name aborts on a same-name campaign in the group that reports a different
  `objectiveType`.
- The settings readback now compares `objective_type` with the objective the recorded strategy
  implies.
- The bid lever refuses `MAX_CLICK` and accepts the manual shape. Adopt, the monitor and the
  conversions metric read no objective, so they are unchanged. A `WEBSITE_VISIT` campaign still
  reports `externalWebsiteConversions`.

**Microsoft default changed from EnhancedCpc to MaxClicks.** A campaign created with no `bidding`
and no `cpcBid` used to get Microsoft's Search default, EnhancedCpc, at the account-currency
minimum ad group bid. The bid lever accepted it. It now gets MaxClicks, which is automatic, so the
lever refuses it (409). The "Budget and Bid Strategies" guide lists MaxClicks for Search and
requires conversion tracking only for MaxConversions, TargetCpa and TargetRoas. MaxClicks' optional
`MaxCpc` is withdrawn for new campaigns from 2027-01-12 (BiddingScheme reference), and this service
does not send it. The help article on location limits could not be retrieved, so no known
constraint is violated, but that is unverified. The legacy `cpcBid` without `bidding` still means a
manual bid at that amount (EnhancedCpc), and is recorded as manual. With `automatic`, `cpcBid` is
refused. With `manual`, it must equal `bidAmount`.

A bad `bidding` is a synchronous `400 invalid campaign config: <configKey>.bidding: ...`. The
check runs in each lane dispatcher's new `PreflightCreate` (Reddit's existing one gains it) and
again in `Dispatch`, before any upstream call. `googleAdsConfig.bidding` is refused 400 at the
service layer; no Google code changed. Migration `000047` adds `campaigns.bidding_strategy`. A
manual create also writes its starting bid to `max_cpc_bid`. The upsert fills both columns only
while they are empty. The campaign resource now returns `bidding_strategy` and `max_cpc_bid`.
`000043`–`000046` belong to the open `fix/LFXV2-2665-adoption-binding-indexes` PR. The gap is
recorded in `allowedVersionGaps`, and whichever PR merges second may need to renumber.
