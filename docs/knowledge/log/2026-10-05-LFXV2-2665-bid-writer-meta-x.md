# 2026-10-05 — LFXV2-2665 bid writer: update-campaign-bid for Meta and X

**Creation** — `update-campaign-bid` (`PATCH .../campaigns/{campaign_id}/bid`) now reaches Meta
and X through the same `BidWriter` contract the Microsoft and Reddit legs follow: one manual max
CPC bid at the level the create path put it, never a strategy switch, provenance and account
identity checked before any call, a read then guards then one classified mutate, and
`max_cpc_bid` persisted only on a confirmed write. Google Ads and LinkedIn still answer 400.

- **Meta** (`internal/dispatch/meta_bid.go`, `internal/platform/meta/bid_update.go`): `bid_amount`
  on the recorded ad set, in the account currency's minor units (`ResolveBidMinorUnits`, the
  create path's currency scale). Written only under `LOWEST_COST_WITH_BID_CAP` with
  `billing_event` and `optimization_goal` both `LINK_CLICKS` — the Marketing API Ad Set reference
  defines the cap per optimization event and, billed on impressions, per 1,000 impressions, so
  only that pairing is a max cost per click. **Every Meta campaign this service creates is
  `LOWEST_COST_WITHOUT_CAP` billed on `IMPRESSIONS`, so it is refused (409)** until an operator
  moves the ad set to a link-click bid cap.
- **X** (`internal/dispatch/twitter_bid.go`, `internal/platform/twitter/bid_update.go`):
  `bid_amount_local_micro` on the recorded line item via a paced, OAuth-signed `PUT`, written
  only for `bid_strategy` `MAX` with `pay_by` `LINK_CLICK`. **Every X campaign this service
  creates is `AUTO`, so it is refused (409)** until an operator moves the line item to a manual
  max bid charged per link click. Provenance is stricter than the X toggle's: a row recording no
  creating account is refused.
- **Throttles are not retried in-call** on either write, so a 429 (or Meta's HTTP-400 rate-limit
  code) is UNCONFIRMED (503) and no refusal returned by a retry can be reported as "nothing
  changed". The X budget write (`UpdateCampaignBudget`) instead retries the 429 and marks a
  later definite failure `retriedUnconfirmedError`; the bid write needs neither.
- Platform refusals of the amount are matched STRUCTURALLY, never on free text or a code
  substring: Meta — a 4xx whose `error_data.blame_field_specs` names `bid_amount` (the Graph
  envelope now parses `error_subcode` and a bounded `error_data`); X — `INVALID_PARAMETER` with
  `parameter == bid_amount_local_micro` (the envelope's bounded `parameter` is now parsed). Each →
  400 with this service's own sentence, never upstream text; any other definite refusal → 503
  "not modified".
