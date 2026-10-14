# 2026-10-13 — LFXV2-2665 Reddit report ledger cleanup

**Fix** — Late PR #306 review (PRRT_kwDOTI9SG86sIeTs): `ReportBudgets.byAccount` in
`internal/platform/reddit/report_budget.go` kept a ledger for every ad account ever seen, and only
the account taking a slot was pruned, so account churn grew memory without bound. Now a ledger
left empty by a release is deleted, and at most once per window `tryTake` sweeps every account
(`sweepLocked`), pruning expired slots and deleting empty ledgers, under the existing lock. The map
holds only accounts read in the last two windows. Test:
`TestReportBudgets_InactiveLedgersAreDropped`. See
[internal/platform/reddit](../code/internal-platform-reddit.md).
