# 2026-08-18 — LFXV2-3260 Microsoft report Scope is a documented UNION

**Note** — `submitReport` in `internal/platform/microsoft/metrics.go` populates BOTH
`Scope.AccountIds` and `Scope.Campaigns`. Microsoft's documentation states these are UNIONed,
which would make this campaign-scoped read return ACCOUNT-WIDE totals. **No code was changed**
— this is recorded for a decision, not fixed, because it changes read semantics.

The behavior is documented rather than inferred. `AccountThroughCampaignReportScope` — the
type of `CampaignPerformanceReportRequest.Scope` — carries the identical sentence on BOTH its
elements:

> The report scope includes a union of the AccountIds and Campaigns elements. You must
> include at least one of these elements.

Verified against Microsoft's own doc source
(`MicrosoftDocs/Advertising`, `bingads-13/reporting-service/accountthroughcampaignreportscope.md`)
as well as the rendered Learn page, to rule out a rendering artifact. The constraint is a
LOWER bound only ("at least one"); there is no mutual exclusion and no precedence rule. The
XSD agrees — both elements are `minOccurs="0"` inside an `xs:sequence`, not an `xs:choice` —
and the reporting error codes 2027–2031 cover invalid values and per-element count limits
only, with no code for specifying both.

If this is right, the consequence matches the failure class the rest of this file is built to
refuse: the request stays valid, no error is raised, and the report simply contains every
campaign in the account. A silently wrong number, not a failure.

**Why it was not fixed here.** Two things are unresolved, and the file's own UNVERIFIED
CONTRACT banner applies — no Microsoft credentials were available to exercise either:

1. Several Microsoft Q&A threads report error 2027
   (`InvalidAccountThruCampaignReportScope`) when `AccountIds` is omitted and only
   `Campaigns` is sent. Those are community posts, not normative docs, and the surrounding
   discussion points at XML namespace/serialization mistakes rather than a real requirement
   — but dropping `AccountIds` on that basis risks trading a wrong number for a hard failure
   on every read.
2. `TestGetCampaignMetrics_SubmitBodyShape` asserts the current both-populated shape, and
   `client.go` documents the account id reaching the body via `Scope.AccountIds`. Changing
   the scope touches that contract and its test.

The narrow fix — send only `Scope.Campaigns` — is a one-line change. It should be made
against a live account, or with the 2027 risk explicitly accepted, rather than on
documentation alone while the gate `MICROSOFT_METRICS_ENABLED` still defaults to false.
