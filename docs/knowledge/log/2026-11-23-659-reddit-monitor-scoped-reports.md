# 2026-11-23 — #659 Reddit account monitor reports only the project's campaigns

**Fix** — The Reddit account monitor sent a report for every ACTIVE/PAUSED campaign on the shared
LF system account (every foundation's), and `Orchestrator.ReadAccountCampaignMetrics` kept only the
project's rows afterwards. In prod every tlf read since 18 Nov came back 7 of 7 `throttle`. The APM
trace of the 20 Nov 13:22 UTC read shows 60 ledger Takes in about 2.5 s for 7 visible rows, with no
other reporting traffic on the account. That fits about 15 reports × up to 4 attempts, each answered
429 by Reddit — not the ledger refusing them.

Now the orchestrator passes the scope to a dispatcher that implements
`service.ScopedAccountMonitorReader` (Reddit: `ReadAccountMonitorIn` →
`reddit.Client.ListAccountCampaignsIn`), and only the project's campaigns get a report. The rows are
still filtered afterwards.

The read also writes one warning per read when reports were throttled, counting Reddit 429s and
ledger refusals apart and naming the first 429's numeric `Retry-After` / `X-Ratelimit-*` headers
(`apiError.RateLimit`). The response body stays unlogged. This is so the cause shows in prod if
Reddit still answers 429 for the smaller read.
