# 2026-08-18 — LFXV2-3260 Microsoft report scope: the union, and the tradeoff taken

**Fix** — Two defects in `internal/platform/microsoft/metrics.go`. The first is a wrong
NUMBER and is fixed. The second cannot be fixed in this client, so it is documented
truthfully instead of being papered over — the honest failure mode is the deliverable.

## Scope was a documented UNION, so a campaign read returned account-wide totals

`submitReport` populated BOTH `Scope.AccountIds` and `Scope.Campaigns`.
`AccountThroughCampaignReportScope` — the type of `CampaignPerformanceReportRequest.Scope` —
carries the identical sentence on both of its elements:

> The report scope includes a union of the AccountIds and Campaigns elements. You must
> include at least one of these elements.

Verified against Microsoft's own doc source (`MicrosoftDocs/Advertising`,
`bingads-13/reporting-service/accountthroughcampaignreportscope.md`) as well as the rendered
Learn page, to rule out a rendering artifact. The constraint is a LOWER bound only ("at least
one"); there is no mutual exclusion and no precedence rule. The XSD agrees — both elements
are `minOccurs="0"` inside an `xs:sequence`, not an `xs:choice`.

The consequence matched the failure class this file refuses everywhere else: the request
stays valid, no error is raised, the report simply contains every campaign in the account,
and `foldReportRows` sums them into an account-wide total reported as one campaign's metrics.
A silently wrong number, not a failure.

`AccountIds` is now dropped. The nested `AccountId` inside `Campaigns[]` already scopes the
request, so nothing is lost.

## The tradeoff we took, stated plainly

Several Microsoft Q&A threads report error 2027 (`InvalidAccountThruCampaignReportScope`)
when `AccountIds` is omitted and only `Campaigns` is sent. Those are community posts, not
normative docs — the documented requirement is only "at least one of these elements" — and
the surrounding discussion points at XML namespace/serialization mistakes rather than a real
requirement. No Microsoft Advertising credentials exist to settle it; the file's own
UNVERIFIED CONTRACT banner applies, and `MICROSOFT_METRICS_ENABLED` still defaults to false.

**We chose a correct-but-possibly-rejected request over a request that silently returns the
wrong number.** A rejection is loud, immediate, and fixable in one line; a wrong number is
none of those and reaches a dashboard as a measurement. That is the same ordering every other
guard in this file already applies.

To keep the rejected case cheap to diagnose, `submitReport` inspects the submit failure and,
when the `apiError` carries code 2027 or the symbolic `InvalidAccountThruCampaignReportScope`,
returns an error naming the campaign-only scope, saying `Scope.AccountIds` may be required
after all, and pointing at this entry. Whoever first runs this against a live account learns
the cause in one read instead of debugging it. Both spellings are matched because Microsoft
returns a numeric `Code` on some services and a string `ErrorCode` on others — the same dual
spelling `errCodeDuplicateCampaign` already handles in `campaign.go`.

Pinned by `TestGetCampaignMetrics_SubmitBodyShape` (asserts `AccountIds` is ABSENT from the
WIRE BYTES as well as from the decoded map — the wire check is the load-bearing one because
`encoding/json` collapses distinct wire forms onto the same decoded value, the same reason
the neighbouring `CampaignId` typing assertions read raw bytes), `TestSubmitReport_Error2027NamesTheScopeTradeoff`
(all three body shapes: numeric `Code`, string `ErrorCode`, nested `Errors` array), and
`TestSubmitReport_UnrelatedErrorDoesNotClaimTheScopeCause` (an unrelated failure must NOT be
annotated with a scope diagnosis it has no evidence for — a misleading cause is worse than a
bare status).

## `ErrReportNotReady` cannot be resumed, so it no longer implies it can

`reportID` is a local in `GetCampaignMetrics` and is discarded when `pollReport` returns
`ErrReportNotReady`. Every retry therefore calls `submitReport` afresh and starts a NEW
Microsoft report job; it never polls the one that has since finished. A report that RELIABLY
exceeds `reportPollBudget` (15s) is thus permanently unreadable through this path however
many times the caller retries — each attempt restarts the clock. Retrying only helps when the
build time varies around the budget. Meanwhile the sentinel's whole framing was "come back
shortly", and the dispatcher said "retry shortly".

**Not fixed, deliberately, and not fixable here.** Returning the pending id alongside the
sentinel does not help on its own: `service.MetricsReader.ReadMetrics` answers
`(*model.CampaignMetrics, error)`, the orchestrator never persists anything from a metrics
read (it is documented as "a pure read, never persisted"), and there is no caller that could
hold the id between attempts. Real resumption needs the pending `ReportRequestId` to outlive
the call — a persisted report-job row plus an async completion path. That is a schema and
orchestration change, out of scope for this client, and this entry is the filed gap.

What changed instead is that nothing now claims a retry works:

* `ErrReportNotReady`'s own message reads "a retry starts a NEW report; it does not resume
  this one" — the sentinel TEXT, not only a doc comment, because the text is what a caller
  reads in a log.
* Its doc comment states why the id cannot survive the call and what building resumption
  would take.
* `GetCampaignMetrics`'s doc says the id is deliberately not returned, and not to add a
  comment claiming otherwise.
* The file header's "an ordinary wrapped error the caller can retry" now says retry means
  "submit a fresh report and hope it builds faster", NOT "collect the one already building".
* `internal/dispatch/microsoft.go`'s error message replaces "retry shortly" with the full
  statement that retrying submits a new report and will not pick up the pending one.

Pinned by `TestErrReportNotReady_DoesNotPromiseAResumableReport` (the sentinel text) and
`TestGetCampaignMetrics_RetryAfterNotReadySubmitsAFreshReport` (two timed-out calls issue two
DISTINCT submits and the retry never polls the first call's `rr-1` — the defect stated as an
assertion, so if resumption is ever built this is the test that must change).

While correcting that comment, the dispatcher's `ReadMetrics` doc was found to claim
`ErrReportNotReady` "is mapped to `domain.ErrMetricsUnavailable`". No such symbol exists in
`internal/domain/errors.go`, and the code immediately below the comment maps it to neither
sentinel. The doc now describes what the code does.

## Mutations

Six, each a COMPILING change, each caught:

1. Re-added `"AccountIds": []json.Number{json.Number(c.account.AccountID)}` to the Scope map
   → `TestGetCampaignMetrics_SubmitBodyShape` fails on both the raw-byte and decoded-map
   assertions.
2. Sent `"AccountIds": nil` instead → BOTH assertions fail. Worth recording precisely,
   because the first draft of this entry predicted the decoded-map check would pass here and
   that was WRONG: `_, ok := scope["AccountIds"]` is a key-presence test, and a JSON `null`
   still creates the key with a nil value. The map check does miss a DIFFERENT case the wire
   check catches — a value that decodes identically from two distinct wire forms, which is
   exactly the quoted-vs-bare id problem the neighbouring `CampaignId` assertions exist for.
   The wire-byte assertion is kept as the load-bearing one for that reason, not for this one.
3. Dropped the 2027 arm's `hasErrorCode` guard so every submit failure is annotated →
   `TestSubmitReport_UnrelatedErrorDoesNotClaimTheScopeCause` fails.
4. Matched only `msErrCodeInvalidScope` (numeric) and not the symbolic name →
   `TestSubmitReport_Error2027NamesTheScopeTradeoff/string_ErrorCode` fails.
5. Reverted `ErrReportNotReady`'s message to the original text →
   `TestErrReportNotReady_DoesNotPromiseAResumableReport` fails.
6. Mutated the FAKE rather than the source, to prove the retry test can actually observe
   resumption instead of passing vacuously: the submit handler returns the same `rr-1` on
   every call, as a server that resumed would →
   `TestGetCampaignMetrics_RetryAfterNotReadySubmitsAFreshReport` fails on the "the retry
   polled rr-1" assertion. Without this, a test asserting that something does NOT happen
   would stay green even if the assertion were unreachable.

## Supersedes

`2026-08-18-LFXV2-3260-scope-union-finding.md` recorded the union as an unfixed finding
awaiting a decision. The decision is made and recorded here; that entry is left untouched as
the record of what was known at the time.
