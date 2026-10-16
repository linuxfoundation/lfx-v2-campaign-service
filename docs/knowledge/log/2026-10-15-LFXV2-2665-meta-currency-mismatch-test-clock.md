# 2026-10-15 — The Meta currency-mismatch test pins its clock and a majority case

**Fix** — two review threads on #316 (LFXV2-2665); tests only, no behaviour change.

- `TestListAccountCampaigns_InsightsCurrencyMismatchLeavesBudgetsUnknown` built its client without
  `WithClock`, so its 30-day window followed the wall clock while the ad-set fixture's flight is
  October 2026: from December 2026 the agreeing-currency control would lose its pacing verdict with
  no code change. The client now pins the same clock `listMonitorRowsAt` uses.
- A three-row case (two `USD` rows and one `JPY` row against a `USD` account) pins that ANY readable
  conflicting row voids the budget currency; a majority-vote guard passes every 1-vs-1 case but
  fails this one.
