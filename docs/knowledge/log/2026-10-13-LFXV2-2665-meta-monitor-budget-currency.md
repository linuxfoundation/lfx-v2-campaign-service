# 2026-10-13 — Meta monitor paces ad-set budgets in the account currency (LFXV2-2665, #235)

**Fix** — Three Meta money-correctness bugs. (1) The Meta account monitor read only
campaign-level budgets, but `CreateCampaign` puts the budget on the ad set, so every campaign this
service creates read `pacing_unknown` and could never raise an under/overspend item. When an
admitted campaign has neither campaign-level budget, `fetchAccountAdSetBudgets` makes ONE
account-level paged read (`GET /act_{id}/adsets?fields=id,campaign_id,daily_budget,lifetime_budget,effective_status`,
20 × 500 cap, `identityjson` before decode) and sums the `ACTIVE` ad sets per campaign; mixed
daily/lifetime, an explicit `null`, an active ad set with no budget, a page-cap hit or an
untrustworthy body all leave those campaigns unknown, never a partial sum; a failed request still
fails the endpoint. (2) `minorUnitsToWhole` divided by 100 unconditionally, reading JPY/KRW budgets
100x too small; the monitor now reads the account currency once per request and converts with its
own offset (unknown currency → budgets unknown). (3) `budgetToMinorUnits` and `bidToMinorUnits`
rounded, so a ¥1.4 budget or bid was written as ¥1 and answered 200 (#235); an amount finer than
one minor unit is now refused with a 400 before any write ("enter whole amounts for this
currency" / "enter at most 2 decimal places"), via a decimal check immune to float representation
error. The encoder is shared, so the create path refuses the same amounts. No API, design, gen or
migration change; `rules/monitor_meta.go` unchanged. See `architecture/account-monitor-endpoints.md`
and `code/internal-platform-meta.md`.
