# 2026-08-14 — LFXV2-3260 Microsoft metrics

**Update** — Microsoft Advertising was the only ad platform with no metrics read at
any layer: no `GetCampaignMetrics` on the client, no `ReadMetrics` on the
dispatcher, so `MicrosoftDispatcher` failed the orchestrator's `MetricsReader`
type assertion and a Bing campaign was invisible on the dashboard once launched.
The other five platforms all had it. Now implemented, default-OFF.

**Microsoft's reporting pipeline is unlike every other platform's here.** The
other five answer with one synchronous JSON request. Microsoft's is three steps —
`POST Reporting/v13/GenerateReport/Submit` returns a `ReportRequestId`, `Poll`
reports `Pending`/`Success`/`Error`, and a pre-signed storage URL then serves a
**ZIP containing a CSV**. It is also on a THIRD host (`reporting.api`), alongside
`campaign.api` and `clientcenter.api`; `doReportingRequest` joins `doRequest` and
`doCustomerRequest` as the third service helper over the shared `do`.

**The synchronous interface is what forced the design.** `ReadMetrics` is called
by the live-read endpoint, and the platform ingress times out at 60s, so the
submit+poll phase is bounded at 20s and gives up with `ErrReportNotReady` rather
than hanging the caller. The download is deliberately OUTSIDE that budget: once
`Success` is reported the file exists, so cutting off the transfer would discard a
report we already paid to build.

**A report still building is not "no data".** `ErrReportNotReady` is mapped by the
dispatcher to an ordinary error rather than to either metrics sentinel, because
both existing sentinels (`ErrMetricsUnsupported`, `ErrMetricsWindowUnsupported`)
mean 400 — "this cannot work" — and a retryable timing condition is neither
unsupported nor permanent. A retryable-metrics sentinel does not exist today;
adding one is a domain change left for a separate decision. Reporting zeroes here
would be the worse failure: a timing condition rendered as a measurement.

**One correction to the gap report that prompted this.** The analysis circulated
on 2026-08-13 said Bing's reporting API is "async SOAP, incompatible with the
service's synchronous metrics design". It is asynchronous, but it is **REST/JSON
at v13**, not SOAP — which is why it fits behind the existing interface at all.

**A realistic fixture found a real bug before any credential did.** Microsoft's
report CSV is RAGGED: a two-column metadata preamble (`"Report Name:"`, …), then
the four-column header and data, then a one-column `©` trailer. Go's `csv.Reader`
locks the field count to the first record by default, so it rejected the whole
file at the header row with "wrong number of fields" — **every** real report would
have failed to parse. Fixed with `FieldsPerRecord = -1`; row width is still
enforced per row by the column lookups. The three tests using the realistic
fixture caught it; the ones with clean CSVs passed, which is exactly why the
fixture models the preamble and trailer rather than a bare table.

**Columns are resolved by header NAME, not position.** Microsoft's report writer
emits columns in its own order, and a positional read would silently swap Clicks
and Spend — producing plausible numbers that are simply wrong. A missing metric
column is refused rather than defaulted to zero, because a zero from an absent
column is indistinguishable from a measured zero.

**The download request deliberately carries no bearer token.** The URL is a
pre-signed storage URL, not an API endpoint: it authorizes itself via its query
string, so attaching our OAuth token would disclose an API credential to a storage
host that neither needs nor expects it. Pinned by a test.

**Default-off, and the flag is wired end to end.** `MICROSOFT_METRICS_ENABLED`
follows `REDDIT_METRICS_ENABLED` exactly, for the same reason: no Microsoft
Advertising credentials were available, so the request/response shapes come from
published documentation and have NOT been exercised against a live account. A
guessed read that returns 200 looks authoritative to every consumer, and the
caveats live only in code comments the response never carries. The chart's
`TestEveryConfiguredEnvVarIsWiredInTheChart` caught the flag being added to
`pkg/constants` without a `values.yaml` entry — without that the gate could never
be flipped in a deployment and the feature would have been permanently
unreachable.

**Verified against a fake that speaks the documented protocol** — real ZIP, real
ragged CSV, submit/poll/download driven end to end — with four guards
mutation-tested: reverting the ragged-CSV fix, reading columns positionally,
attaching the bearer token to the download, and treating an unknown status as
Pending (that last one HANGS, which is the hazard the guard exists to prevent).
That is not the same as verification against Microsoft: the live contract remains
unconfirmed, which is what the flag is for.
