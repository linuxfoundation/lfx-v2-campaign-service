# 2026-08-18 — LFXV2-3260 Microsoft metrics: pre-review fixes

**Fix** — Four defects found by pre-PR reviewer simulation on cs#140, all reproduced before
being fixed and all mutation-verified after.

**The pre-signed download URL leaked into two error paths.** `downloadReport` wrapped the
transport cause with `%w`, and `net/http` returns a `*url.Error` whose `Error()` renders the
full URL — including the `sig=` query parameter that IS the credential. A probe against an
unroutable host printed the signature in plaintext. This is the disclosure the file's own
comments guard against on the *status* path, and the convention `client.go`'s `do()` states
explicitly when it carries `path` separately from `fullURL`. Both arms now return a
classification only, never the cause. `TestDownloadReportErrorsOmitThePresignedURL` pins it;
reverting to `%w` fails it with the signature visible.

**`ErrReportNotReady` was unreachable in production.** `reportPollBudget` was 20s, and the
binding deadline is not the 60s ingress the comment cited — `Orchestrator.ReadCampaignMetrics`
wraps every metrics call in `metricsCallTimeout`, which is *also* 20s. The caller's context
therefore cancelled at or before the budget check, so the sentinel, its message and the
dispatcher's classification arm were all dead code: a slow report surfaced as a bare
`context deadline exceeded`. Budget reduced to 15s and the comment now names the real
constraint. `TestReportPollBudgetStaysUnderTheMetricsCallTimeout` fails if the two ever meet
again.

**The budget branch was untestable, not merely untested.** Every test built its client with a
*constant* clock, so `c.now().Add(reportPollInterval).Before(deadline)` compared a frozen now
against a deadline derived from that same frozen now — permanently true. Added an advancing
clock and `TestGetCampaignMetrics_BudgetExpiryReturnsNotReady`. Mutation-verified with a
COMPILING change (pushing the deadline out 24h rather than deleting the check, which only
broke the build): the test hangs to its timeout, which is the exact failure the budget exists
to prevent.

**The dispatcher gate had no test, and `ReadMetrics` was missing from the connection-defect
suite.** `MICROSOFT_METRICS_ENABLED` is the only thing between an unverified contract and
numbers a dashboard renders as authoritative — an untested gate is the one most worth pinning.
Added `internal/dispatch/microsoft_metrics_test.go` covering the disabled path, a table that
fails closed on `"TRUE"`/`"1"`/`"yes"`/whitespace-padded values, and a positive case proving
the gate is a gate rather than an unconditional refusal. Registered `ReadMetrics` in
`TestMicrosoft_UnusableConnectionIsTaggedOnEveryPath` so a bad credential on the new path is
tagged as a 409 rather than surfacing as an opaque 500.

**Docs corrected.** `docs/api-catalog.md` and `docs/knowledge/code/internal-dispatch.md` both
still said Microsoft metrics were unsupported, and both called the Reporting API SOAP. It is
REST/JSON at v13. The `internal-dispatch.md` paragraph also instructed the next engineer that
closing the gap was "deferred, not attempted here" — the document someone would read before
attempting exactly this work.

Also replaced two "the other five" counts with a description of the shape: there are seven
`ReadMetrics` implementers, so the enumeration was already wrong, and the next platform would
have falsified it again.
