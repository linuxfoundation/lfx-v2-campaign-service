# 2026-10-28 — verify-metrics: a -read selector covers every gated read

**Update** — `cmd/verify-metrics` now verifies every read that is switched off in production, not
only campaign metrics. A new `-read` flag selects the read. It defaults to `metrics`, so existing
commands behave as before.

- `audience`: Reddit (`ReadRedditAudienceInsights`), Microsoft (`ReadReportedAudience`) and X
  (`ReadTwitterAudienceInsights`).
- `keywords`: Reddit (`ReadRedditKeywordPerformance`) and Microsoft
  (`ReadReportedKeywordPerformance`).
- `account-monitor`: Microsoft and X (`ReadReportedAccountCampaigns`, then the endpoint's rule
  engine).

Each mode calls the `service.Orchestrator` method its endpoint handler calls, on the command's
read-only backend. Flags use the endpoint payload's names and rules: the window subset and
default, `-account-id` and `-days` (7–90), and refusal of the system scope. The report-backed
reads keep their saved reports in process memory and poll until the report finishes. A read
that never contacted the platform (an empty scope) exits 3, not 0. The X reads take the
service's stats-job lease, so a run never creates X stats jobs alongside a service pod.

The transport allowlist is now per read and provider, and lists each POST by name. The new POSTs are
Microsoft's `Campaigns/QueryByAccountId` and `Account/Query` for the account monitor, and X's
stats-job create for the X monitor and audience read. The service's log lines and the new result
output are redacted. Write selectors (`keyword-targeting`, `keyword-actions`, `negative-keywords`)
are refused with the reason. The Reddit account monitor and an X keyword read are refused because
neither is gated. [cmd/verify-metrics](../code/cmd-verify-metrics.md) is now a runbook with one
command per switched-off feature and what to compare it against in the vendor's UI.
