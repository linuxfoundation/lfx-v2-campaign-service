# 2026-11-04 — A zero ad-schedule bid modifier is refused before the budget

**Fix** — `validateAdSchedules` used to accept an ad-schedule `BidModifier` of exactly 0 as
a "do not serve" opt-out, because the schedule check reused the device rule in
`validateBidModifier`. That opt-out is valid for DEVICE criteria only. Google's
CampaignCriterion contract admits 0.1–10.0 for an ad-schedule bid modifier, so a 0 passed the
preflight and then failed the criteria mutate after the budget and campaign existed. Copilot
raised it on lfx-self-serve #5088, whose targeting panel could send a −100% schedule
adjustment as 0.

`validateBidModifier` now takes an `allowZero` argument, and the 0.1..10.0 range stays one
shared rule. Schedules pass `false`: a 0 is refused before any Google Ads request with
`ad schedule <i> bid modifier 0 must be between 0.1 and 10 (Google has no 0 opt-out here);
omit BidModifier to serve at the normal bid, or leave the interval out to stop serving then`.
Devices pass `true` and keep 0 as the exclusion. Those are the only two callers.

Unchanged: Demand Gen still refuses any schedule modifier, with its own message, before the
range check runs. `validateBiddingPlan`'s smart-bidding arm still keys on a non-zero device
modifier only. Performance Max still refuses device criteria. No validateOnly probe of a 0
schedule modifier is recorded in this repo; the rule follows Google's documented contract.

The api-catalog `adSchedules.bidModifier` range, the googleads and dispatch concepts, and the
`minBidModifier` and pointer comments now say that 0 is a device-only value.
