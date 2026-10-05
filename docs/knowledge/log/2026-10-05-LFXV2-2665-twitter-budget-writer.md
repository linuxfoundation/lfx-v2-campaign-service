# 2026-10-05 — LFXV2-2665: X Ads campaign budget writer

**Creation** — `TwitterDispatcher` now implements `BudgetWriter`, so
`PATCH .../campaigns/{id}/budget` works for X alongside Google Ads, LinkedIn, Meta, Microsoft
Advertising and Reddit. No service-layer change was needed: support is decided by the type
assertion alone. The `update-campaign-budget` design description now names six platforms and X's
model, and `gen/` was regenerated.

## Where the budget lives

INFERRED from this repo's own create path, and unverified against a live account:
`CreateCampaign` sends `daily_budget_amount_local_micro` on the **campaign**, no
`budget_optimization` (X's v11 announcement makes `CAMPAIGN` the default; the current reference
lists `LINE_ITEM` as the only value), and no budget on the line item. The writer does not rely on
the inference: it writes only when X REPORTS `CAMPAIGN`. The write is one
`PUT accounts/:account_id/campaigns/:campaign_id` with exactly
`daily_budget_amount_local_micro`, OAuth 1.0a-signed and paced on the account's shared write
pacer ([reference](https://docs.x.com/x-ads-api/campaign-management/reference), "Campaigns").

**Daily only.** Under `CAMPAIGN` X requires the daily budget on the campaign, and the create path
never sets a total, so a `lifetime` request is refused (409) before any call; no cited X document
shows a total-only `CAMPAIGN` campaign.

## Guards, all before the PUT

Provenance failed closed before any call (X has no fallback for rows lacking `AccountID`); the
amount through the create path's own bound and rounding (`twitter.BudgetMicros`, 400 on refusal —
X publishes no per-currency minimum); the lifetime refusal; the shared resolution and cached
client; a pure GET whose 404 or `deleted: true` is 404. Then `budget_optimization` must be
reported as `CAMPAIGN` — `LINE_ITEM`, unreported or unknown is refused (409) — and the campaign
must be daily-only (a daily amount set, no total; total-only, neither, both, or an unreadable
amount → 409). No shared-budget analogue exists.

## Outcome classification

A 429 is retried (the write converges). Transport failure, mutating 3xx, exhausted 429 and 5xx
are UNCONFIRMED (503). Any failure after a retried 429 — a definite 4xx, or even a pre-send dial
failure — is UNCONFIRMED too:
`doRequestAbsCounted` counts retries in a caller-owned counter and `UpdateCampaignBudget` wraps
such a refusal in `retriedUnconfirmedError`, mirroring the Microsoft fix in PR #255. The 2xx echo
is checked against the campaign id and the amount sent, and a test verifies the OAuth signature
covers the query parameter.

## Not gated, and what is unverified

X campaign writes (create, toggle) are already ungated, so no flag was added;
`TWITTER_METRICS_ENABLED` still gates only the account monitor. Unverified against a live
account: the `budget_optimization` value a created campaign reads back as, whether the GET
returns deleted campaigns, whether the PUT echoes the campaign, and X's unpublished minimums —
each fails closed.
