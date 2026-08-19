# 2026-08-18 — LFXV2-3260 Microsoft metrics: header-only report, and a test race

**Fix** — Two findings from PR #140 review. One is the same failure class this file refuses
everywhere else — absence rendered as a measured zero — arriving through a door that was
still open. The other is an unguarded capture in a new test, fixed on the strength of the
code being plainly wrong rather than on a reproduction, because the reproduction does not
exist and the reason it does not is itself worth recording.

**A header-only report returned a measured zero.** `GetCampaignMetrics` goes to real lengths
to refuse absence-as-zero on the poll side: a `Success` status carrying no `ReportDownloadUrl`
answers `ErrNoRowsInReport`, with a comment explicitly declining to assume Microsoft omits the
file rather than shipping a header-only CSV — "a claim about response SHAPE on a contract this
file declares UNVERIFIED". But if Microsoft does ship that header-only CSV, the download path
had no equivalent refusal. `reportHeaderAndRows` finds the header, `dropTrailerRows` removes
the `©` line, and `foldReportRows` folds over an EMPTY row set: the loop body never executes
and the function returns a zero-valued `CampaignMetrics` with a nil error. A successful,
all-zero measurement synthesized from a file that measured nothing.

The missing-columns guard immediately above does not already cover this, and that was checked
before anything was changed rather than assumed: a header-only file NAMES `CampaignId`,
`Impressions`, `Clicks` and `Spend`, so all three lookups succeed and the guard passes. A
probe calling `foldReportRows` with exactly those records returned
`&{Impressions:0 Clicks:0 CostMicros:0 Ctr:0}, <nil>` — the finding is real and reachable, not
a false positive shadowed by an earlier guard.

`foldReportRows` now returns `ErrNoRowsInReport` when `len(rows) == 0`, making the two doors
symmetric. The same sentinel is correct for both because the two shapes carry identically
little information: in neither can this adapter distinguish "the campaign served nothing" from
"no such campaign in this account's scope". `ErrNoRowsInReport`'s doc comment said "the poll
reported Success but named no file to download", which is now only half of what it means; it
was rewritten to name both shapes, since the sentinel's doc is what a reader consults to learn
what the sentinel covers. `internal/dispatch/microsoft.go` was confirmed to already map it onto
`domain.ErrNoMetricsInWindow` — the mapping is on the sentinel, not on the call site, so the
new door inherits it with no dispatcher change.

Pinned at both levels: `TestGetCampaignMetrics_HeaderOnlyReportIsNotAZero` drives a real ZIP
containing a real header-only CSV through the whole pipeline, and
`TestFoldReportRows_HeaderOnlyIsNotAZero` pins the guard at the unit it lives in so a refactor
of the ZIP/CSV plumbing cannot quietly drop it.

**A test captured concurrent state without a mutex — but -race does not catch it, and the
reason matters.** In `TestGetCampaignMetrics_RetryAfterNotReadySubmitsAFreshReport` the Submit
handler did `submits++` and the Poll handler did `polledIDs = append(polledIDs, …)`, both
unguarded, both then read from the test goroutine. Every sibling capture in this file is
guarded by `msMetricsServer.mu`; this was the sole exception. Both are now guarded by a local
mutex, including the final reads.

The mutation test did NOT fire, and that is the finding worth keeping. Removing the
`polledIDs` lock and running `-race -count=3` passed clean; the unmutated test at `-count=20`
also passed clean (roughly 240s of race-instrumented runtime). The reason is that
`net/http.Server` serves all requests on ONE connection from ONE goroutine, and this client
issues its requests strictly sequentially: a probe counting distinct handler goroutines across
20 sequential `httptest` requests reported exactly **1**. So the handlers never actually run
concurrently here, and the "many goroutines, one per poll" premise behind the report is wrong
— `httptest` spawns a goroutine per CONNECTION, not per request.

That does not make the guard pointless, and it is the reason to write this down rather than
revert. The safety of the unguarded version rests on an invariant nothing states or enforces:
connection reuse in `net/http`'s transport, plus the client never issuing an overlapping
request. Either can change without touching this test — a `t.Parallel()` sub-test, a second
concurrent call added to assert something else, a keep-alive disabled, a retry that opens a
fresh connection — and the failure would then appear as an intermittent CI flake in a test
whose subject is unrelated. The guard costs two lines and removes the dependency on that
invariant. Recorded explicitly so a future reader who runs the mutation, sees it pass, and
concludes the mutex is cargo-cult has the measurement and the mechanism in front of them.

**Not changed: `reportPollInterval = 1s`.** Review asserted Microsoft recommends 2–15 MINUTE
polling intervals, which would make a 1s interval both throttling-prone and pointless against
a 15s budget. That assertion is CORRECT and is now verified against the primary source —
[Request and Download a Report](https://learn.microsoft.com/en-us/advertising/guides/request-download-report?view=bingads-13),
step 6: "Because most reports should complete within minutes, polling at two to 15-minute
intervals should be appropriate for most cases. If the overall polling period exceeds 60
minutes, consider saving the report identifier, exiting the loop, and trying again later."
The existing code comment claiming "Microsoft documents a recommended floor of ~1s" is not
supported by that page.

The interval was still left alone, deliberately, because the documentation invalidates far
more than the constant. If reports take MINUTES, then a 15s `reportPollBudget` essentially
never observes a completed report, and the synchronous `MetricsReader` contract is the real
problem: the budget cannot be raised to minutes because `metricsCallTimeout` is 20s and the
platform ingress is 60s. Microsoft's own advice for this case — persist the ReportRequestId,
exit, come back later — is precisely the resumable-report capability `ErrReportNotReady`
already documents as absent and as needing a schema plus async completion path. Raising 1s to
5s would change nothing about that and would only reduce the number of doomed polls inside a
budget that expires regardless. This is a design decision with a live-behavior consequence, so
it is reported rather than taken unilaterally; the path stays gated behind
`MICROSOFT_METRICS_ENABLED` in the meantime.

**Mutation tests.** Header-only guard disabled (`if false && len(rows) == 0`): both
`TestGetCampaignMetrics_HeaderOnlyReportIsNotAZero` and
`TestFoldReportRows_HeaderOnlyIsNotAZero` failed with `err=<nil>` and a zero-valued metrics
struct. Guard returning a plain `fmt.Errorf` instead of the sentinel: both failed on
`errors.Is`, proving they pin the SENTINEL and not merely "some error" — which is the property
the dispatcher's mapping to `domain.ErrNoMetricsInWindow` depends on. `polledIDs` lock removed
under `-race -count=3`: passed, i.e. NOT caught — analysed above.
