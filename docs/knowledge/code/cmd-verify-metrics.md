---
type: "Go Package"
title: "cmd/verify-metrics"
description: "Local, read-only operator CLI and runbook for verifying every gated read before its flag is flipped: campaign metrics (Reddit, Microsoft, X; also LinkedIn and Meta), the Reddit, Microsoft and X audience reads, the Reddit and Microsoft keyword reports, and the Microsoft and X account monitors, each run against a live account through the service's own connection resolution and the method its endpoint calls, with the flag forced on in-process only and every secret redacted."
resource: "cmd/verify-metrics"
---

# cmd/verify-metrics

The first step of decision D8: verify a GATED read against a live ad account BEFORE its deployment
flag is flipped. `REDDIT_METRICS_ENABLED` (#167), `MICROSOFT_METRICS_ENABLED` (#168) and
`TWITTER_METRICS_ENABLED` stay off until someone has seen the real platform answer; this command is
how they see it without turning the endpoint on for everyone.

## Runbook: one command per switched-off feature

Every command needs the service's database and key settings — `DATABASE_URL` or `PG*`, plus
`CREDENTIAL_ENCRYPTION_KEY` (see README "Build and run locally"). `<slug>` is a project that owns
campaigns on the platform in this service. Add `-raw` to see every upstream exchange, redacted.
Money in the output is `CostMicros` (account currency × 10⁶) or `Spend` (account currency).

| Flag it unblocks | Feature | Command | Compare against, in the vendor's own UI |
| --- | --- | --- | --- |
| `REDDIT_METRICS_ENABLED` | campaign metrics | `go run ./cmd/verify-metrics -provider reddit-ads -project <slug> -campaign <t3_ id> -window last_7_days` | Reddit Ads Manager → the campaign, same date range: impressions, clicks, spend. |
| `REDDIT_METRICS_ENABLED` | audience (`GET /projects/{project_id}/reddit-ads/audience`) | `go run ./cmd/verify-metrics -read audience -provider reddit-ads -project <slug> -window last_30_days` | Ads Manager → Reports, the project's campaigns, broken down by Gender, then Country, then Community: impressions, clicks, spend per bucket. |
| `REDDIT_METRICS_ENABLED` | keywords (`GET /projects/{project_id}/reddit-ads/keywords`) | `go run ./cmd/verify-metrics -read keywords -provider reddit-ads -project <slug> -window last_30_days` | Ads Manager → Reports, broken down by Keyword: impressions, clicks, spend per (campaign, keyword). |
| `MICROSOFT_METRICS_ENABLED` | campaign metrics | `go run ./cmd/verify-metrics -provider microsoft-ads -project <slug> -campaign <id> -window last_7_days` | Microsoft Advertising → Campaigns, same date range: impressions, clicks, spend, conversions. |
| `MICROSOFT_METRICS_ENABLED` | keywords (`GET /projects/{project_id}/microsoft-ads/keywords`) | `go run ./cmd/verify-metrics -read keywords -provider microsoft-ads -project <slug> -window last_30_days` | Keywords tab (or Reports → Keyword), same campaigns and range: impressions, clicks, spend, conversions, match type, quality score. |
| `MICROSOFT_METRICS_ENABLED` | audience (`GET /projects/{project_id}/microsoft-ads/audience`) | `go run ./cmd/verify-metrics -read audience -provider microsoft-ads -project <slug> -window last_30_days` | Reports → Age and gender (or the Demographics tab), same campaigns and range: impressions, clicks, spend per age group × gender. |
| `MICROSOFT_METRICS_ENABLED` | account monitor (`GET /projects/{project_id}/connection-microsoft-ads/account-monitor`) | `go run ./cmd/verify-metrics -read account-monitor -provider microsoft-ads -project <slug> -account-id <id> -days 30` | Campaigns page for the account over the last 30 days: names, statuses, daily budgets, spend, impressions, clicks, conversions; Accounts summary for the currency. |
| `TWITTER_METRICS_ENABLED` | account monitor (`GET /projects/{project_id}/connection-twitter-ads/account-monitor`) | `go run ./cmd/verify-metrics -read account-monitor -provider twitter-ads -project <slug> -account-id <id> -days 30` | X Ads Manager → Campaigns over the same days in the account's timezone (the output's `MetricsWindowStart`/`End`): spend, impressions, clicks, budgets, flights. |
| `TWITTER_METRICS_ENABLED` | audience (`GET /projects/{project_id}/twitter-ads/audience`) | `go run ./cmd/verify-metrics -read audience -provider twitter-ads -project <slug> -window last_7_days` | Ads Manager → Analytics → Demographics (age, gender) and the platform/device breakdown, last 7 days: impressions, clicks, spend. |

A match on the live account is the evidence for flipping the flag. A difference is the finding:
rerun with `-raw` and attach the redacted exchange to the issue.

The report-backed reads (Microsoft keywords, audience and account monitor; X account monitor) ask
the platform to build a report, so a run prints `poll: attempt N: report still building` every
`-poll-interval` (default 20s) until it finishes. Their default `-timeout` is 15m, not 3m.

A run that exits 3 with `no upstream request was made` means the project has no campaigns on the
platform in this service: the endpoint answers empty without contacting the platform, so nothing
was verified. Pick a project that has some.

The printed result is the service-layer model the method returns (Go field names), which the
endpoint then renders into its wire shape. The account monitor also prints the endpoint's rule
engine rows and action items, but not the totals the endpoint sums from them. Compare values, not
field names.

## Flags

- `-read`: `metrics` (default, today's behaviour), `audience`, `keywords`, `account-monitor`.
  Gated pairs: audience for reddit-ads, microsoft-ads, twitter-ads; keywords for reddit-ads and
  microsoft-ads; account-monitor for microsoft-ads and twitter-ads. Others are refused (exit 2)
  with the reason: X has no gated keyword read; the Reddit account monitor is a live read that
  is not behind any flag; LinkedIn and Meta have no gated read except metrics.
- **Writes are refused.** `-read keyword-targeting`, `keyword-actions` and `negative-keywords`
  exit 2 with a usage error that explains why: the tool is read-only by construction, so a write
  could not succeed. Verify a write path through the service in a test ad account instead. The
  `-read` help text says the same.
- `-provider`: `reddit-ads`, `microsoft-ads`, `twitter-ads`, `linkedin-ads`, `meta-ads`.
  `google-ads` is refused: its reads are not gated, and its client is out of scope. `hubspot` is
  refused too.
- `-project`: the connection scope key exactly as the service keys it (slug or UUID). For
  `metrics` only, `lf-system` is an alias for `model.SystemProjectID` (`system:linuxfoundation`).
  The other reads refuse the system scope, as their endpoints do (`rejectSystemScope`). A metrics
  read of a project with no connection falls back to the system row as the service does.
  `LFX_FORCE_SYSTEM_ADS_ACCOUNT` does NOT redirect metrics reads; the output notes that.
- `-campaign` (metrics only): the PLATFORM's campaign id. If the project holds a row with that id,
  its provenance is passed along so the dispatcher's account-match guard runs. Otherwise the read
  goes by platform id with no provenance, and the output says which. Several rows with different
  provenance are refused.
- `-window`: for metrics, any `model.MetricsWindow` (default `last_7_days`). For audience and
  keywords, the endpoint's subset and default: Reddit and Microsoft take `today`, `last_7_days`,
  `last_30_days`, `this_month`, `last_month` (default `last_30_days`); X audience takes `today`,
  `yesterday`, `last_7_days` (default `last_7_days`). Refused for account-monitor.
- `-account-id`, `-days` (account-monitor only, both required): the endpoint's `account_id` and
  `days`. The account id is checked with the dispatcher's own `ValidateMonitorAccountID`, and days
  must be 7–90.
- `-poll-interval` (report-backed reads only), `-timeout`, `-raw`.

A flag the selected read does not take is refused rather than ignored. Exit status: 0 success,
1 a failed read or setup, 2 a usage error, 3 nothing verified (an empty scope: no upstream
request was made).

## Same path as the service

The dispatcher is built with the service's own constructor (`dispatch.NewRedditDispatcher` and the
others) on the connection repository and AES-GCM encryptor. Database and key loading mirror the
`bootstrap-system-account` subcommand.

- `metrics` calls the dispatcher's `ReadMetrics`, the `service.MetricsReader` method that
  `Orchestrator.ReadCampaignMetrics` calls for get-campaign-metrics.
- The other reads build a `service.Orchestrator` with that one dispatcher and call the method the
  endpoint handler calls: `ReadRedditAudienceInsights`, `ReadRedditKeywordPerformance`,
  `ReadTwitterAudienceInsights`, `ReadReportedAudience`, `ReadReportedKeywordPerformance` and
  `ReadReportedAccountCampaigns`. So the project scope, the empty-scope early return, the
  account-provenance guards, the merge and the normalisation all run as in the service. The
  account monitor also runs the endpoint's rule engine (`rules.EvaluateMicrosoftMonitor`,
  `rules.EvaluateTwitterMonitor`) and prints its rows and action items.
- The report-backed reads keep their saved-report state in in-process stores
  (`cmd/verify-metrics/memstore.go`) that follow the repositories' contract: the first pending
  mark wins, and complete or fail is a compare-and-set on the report id. A run therefore starts
  from "nothing saved", which is what an account sees the first time its flag is flipped.

The only differences from the service are the in-process flag, the HTTP client (below), the
in-memory report state, and that no platform base URL can be overridden from the command line.
Tests inject one through the unexported `endpoints` seam, so a typo cannot send live credentials
to another host.

## Read-only by construction

- The database pool is the command's own. Every session runs `SET SESSION CHARACTERISTICS AS
  TRANSACTION READ ONLY` (pinned by a live-database test).
- The X dispatcher is bound to the service's own stats-job lease (`postgres.NewStatsJobLease`,
  a session-level advisory lock that a READ ONLY session may take). A run therefore creates X
  stats jobs only when no service pod, and no other run, owns the account, and fails before
  contacting X otherwise. The lease is released at exit.
- The dispatcher gets a connection reader with only `Get` and `Disconnected`, and an encryptor
  whose `Encrypt` is refused. The orchestrator gets a campaign repository whose every write
  returns `errReadOnly`, and in-memory report stores in place of the saved-report tables.
- Every upstream request passes `allowedRequest(read, provider, method, path)` before it is sent.
  GET and HEAD are allowed. A POST is allowed only if it is on the named list for the selected
  read on the selected provider (`postAllowlist` in `capture.go`), so a run can never send a POST
  that only another read, or another platform's client, needs. X metrics admit no POST at all:

  | POST | Why it is a read | Reads |
  | --- | --- | --- |
  | OAuth token exchange | mints an access token, as every service read does | every Reddit and Microsoft read; LinkedIn and Meta metrics |
  | Reddit `/ad_accounts/{id}/reports` | report query over delivery data | Reddit metrics, audience, keywords |
  | Microsoft `GenerateReport/Submit`, `/Poll` | builds and fetches a report file | Microsoft metrics, audience, keywords, account-monitor |
  | Microsoft `Campaigns/QueryByAccountId` | Microsoft's REST API reads by POST; the monitor's campaign list | Microsoft account-monitor |
  | Microsoft `CustomerManagement/v13/Account/Query` | the monitor's currency read | Microsoft account-monitor |
  | X `/{version}/stats/jobs/accounts/{id}` | creates an analytics job that computes stats into a file; touches no campaign | X audience, account-monitor |

  Anything else (a campaign or keyword create, PATCH, PUT, DELETE) fails in-process, and the
  platform never sees it.

## Redaction

Every byte printed goes through a scrubber that holds every secret the run has observed:

- each string value of the credential document the dispatcher decrypted (recorded by wrapping the
  encryptor);
- every credential-bearing request header (Bearer tokens, OAuth 1.0a parameters, Microsoft's
  developer token);
- every secret-named query value a request carried;
- the tokens a token exchange issued.

Each is also recorded in its query-escaped and JSON-escaped forms. The service's own log lines go
through the same scrubber in every mode: the run installs a scrubbing slog handler before the
backend or dispatcher is built (the report-backed reads, for one, log a failed check or
submission and carry on). The audience, keywords and account-monitor
results are printed through the `-raw` body redactor. That means secret-named keys are withheld,
URLs are reduced by `pkg/redact.SnapshotText`, and int64 values keep full precision.

Token exchanges are printed as a status line only. URLs go through `pkg/redact.URL`, with
secret-named query values (`sig`, `*token*`, and so on) withheld. In bodies, every decoded string
has its URLs reduced and every recorded secret scrubbed before re-encoding. The output is encoded
without HTML escaping, so a signed URL is not split at its first `&`. A JSON body that does not
decode whole is withheld. An exchange still unanswered when the read ends is printed as pending. A
token exchange response over the 256 KiB capture cap is refused.
