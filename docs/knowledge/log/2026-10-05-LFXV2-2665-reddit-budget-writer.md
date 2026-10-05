# 2026-10-05 — LFXV2-2665: Reddit Ads campaign budget write

**Creation** — `RedditDispatcher` now implements `BudgetWriter`, so the Optimize tab's
"change a budget" lever (`PATCH .../campaigns/{id}/budget`) works for Reddit alongside Google
Ads, LinkedIn and Meta. No service-layer change was needed: support is decided by the type
assertion alone.

## Where the budget lives

Read from this repo's own create path rather than assumed: `CreateCampaign` sets
`is_campaign_budget_optimization: true`, `goal_type: "LIFETIME_SPEND"` and `goal_value` (integer
micro-units of the account currency) on the **campaign**, and no budget on the ad group. So the
write is one `PATCH /ad_accounts/{accountID}/campaigns/{campaignID}` — the resource the status
toggle already PATCHes — with exactly `{"data":{"goal_value": <micros>}}`. `goal_type` is never
sent, so the pacing cannot change as a side effect.

## Guards, all before the PATCH

Provenance failed closed before any call (Reddit has no fallback for rows lacking `accountId`);
the amount through the create path's own bound and rounding (`reddit.BudgetMicros`, 400 on
refusal); credentials through the same resolver as the toggle; then a read of the campaign. The
read must name the same campaign (and, if it names one, the same ad account); CBO must be
reported and on — with it off the spend is per ad group, and following Meta's precedent an
allocation across ad groups is refused rather than guessed; `goal_value` must be legible; and
`goal_type` must map (`LIFETIME_SPEND` ↔ lifetime, `DAILY_SPEND` ↔ daily) and match the request.
Every refusal there is `ErrBudgetUnwritable` → 409, the siblings' pacing-mismatch sentinel.

## Ambiguity

The PATCH retries a 429 (it converges); an exhausted throttle, transport failure, 3xx or 5xx is
UNCONFIRMED. A 2xx whose echo names another campaign or another `goal_value` is UNCONFIRMED too.
A definite 4xx is a refusal.

## Not verified live

Reddit's OpenAPI document could not be fetched from the build environment, and no request has
been made against a live account. The single-campaign GET on the account-scoped path, whether it
reports `is_campaign_budget_optimization` and `ad_account_id`, the `DAILY_SPEND` token and the
PATCH echo shape are therefore unverified; each fails closed (409 or UNCONFIRMED), never as a
wrong write. Tests: `internal/dispatch/reddit_budget_test.go`,
`internal/platform/reddit/budget_update_test.go`.
