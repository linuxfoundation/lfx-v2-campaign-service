# 2026-10-28 — Google Ads cpcBid is documented where callers look

**Docs** — refs #156. #156 asked for a settable Google Ads CPC bid, so that a manual-CPC Search
campaign is not left at Google's floor bid. #239 already shipped it as `googleAdsConfig.cpcBid`.
It is sent on the ad group as `cpcBidMicros`, validated (0.01 to 100,000) before any Google Ads
request, and leaving it out keeps the previous behaviour. #156 was closed as done by #239.

The bid was hard to find, though. It was documented only in `docs/api-catalog.md`. The published
`config` attribute did not mention it, and the 400 for `googleAdsConfig.bidding` pointed only to
`googleAdsConfig.biddingStrategy`. So a caller who tried the other platforms' `bidding.bidAmount`
on Google was steered away from the field that sets the bid.

## What changed

- `design/brief.go`: the `config` attribute documents `googleAdsConfig.cpcBid`. It covers what
  the bid is, its currency, that it is Search-only under manual CPC, its 0.01 to 100,000 range,
  and that omitting it keeps the previous behaviour. Regenerated with `make apigen`.
- `internal/service/create_bidding.go`: the `googleAdsConfig.bidding` 400 now names both
  `googleAdsConfig.biddingStrategy` and `googleAdsConfig.cpcBid`. `TestRefuseUnsupportedBidding`
  asserts both names.
- `docs/api-catalog.md` and `code/internal-service.md` say the same.

No behaviour changes beyond the wording of that 400 message. Accepting `bidding.bidAmount` on
Google itself remains a separate, undecided product question.
