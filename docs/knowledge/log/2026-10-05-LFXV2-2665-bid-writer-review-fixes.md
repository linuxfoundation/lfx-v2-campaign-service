# 2026-10-05 — LFXV2-2665 bid writer: pre-PR review fixes

**Fix** — three findings from the pre-PR review of the `update-campaign-bid` lever, each a place
where a guard was weaker than the lever's "an unreported fact is refused" rule.

- **Reddit ad-group ownership failed open.** `RedditDispatcher.WriteBid` skipped the ownership
  check when the ad group GET omitted `campaign_id`. An unreported owner is now refused (409)
  like a different one.
- **Reddit amount refusals were matched on raw text.** Any 400 whose body contained "bid_value"
  was told "outside the range Reddit accepts", including a strategy race or an echoed payload.
  `UpdateAdGroupBid` now matches only a structured field error
  (`error.fields[].field == "bid_value"`); anything else stays a definite refusal (503 "not
  modified").
- **Microsoft's portfolio guard could never fire.** `BidStrategyId` is a CampaignAdditionalField,
  so `GetCampaignsByIds` returns it only when asked. The bid read now sends
  `ReturnAdditionalFields: "BidStrategyId"`. The budget read's body is unchanged.
- **Reddit campaign-level strategy is now read too.** Under Campaign Budget Optimization an ad
  group's strategy must match its campaign's, and Reddit's reference could not be fetched to
  confirm more. The bid write therefore reads the campaign first. It refuses unless the
  campaign's own strategy allows a manual ad-group bid, and it checks the campaign's reported
  account.
- Noted as a known gap rather than fixed: on Reddit a definite 4xx that follows a retried 429 is
  classified definite. This is inherited from the budget write.
