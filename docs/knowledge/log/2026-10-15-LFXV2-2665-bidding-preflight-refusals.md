# 2026-10-15 — LFXV2-2665 bidding refusals marked operator-safe after the #208 merge

**Update** — Merging main (#335, issue #208) into the create-time bidding branch left the new
bid refusals unmarked, so a bad `bidding` that reached `Dispatch` would have been reported in the
async job as the generic string. `biddingInvalid` (`internal/dispatch/bidding.go`) now wraps every
`<configKey>.bidding: …` refusal in `domain.PreflightRefusal` (keeping `errors.Is` on
`domain.ErrCreateConfigInvalid`), and `quoteKey` renders caller keys through `domain.CallerValue`.
The clients' own pre-send bid checks are marked too — X `manualMaxBidMicros` and bid-over-budget,
Reddit's manual-bid objective and amount checks, LinkedIn `campaignBidding`, Microsoft
`validateBiddingScheme` (whose unsupported-scheme text now uses `CallerValue`). Meta's bid-cap
checks stay unmarked: they run after the account preflight GET. `preflight_refusal_test.go` gains
an X manual-bid-over-budget case. See [internal/dispatch](../code/internal-dispatch.md).
