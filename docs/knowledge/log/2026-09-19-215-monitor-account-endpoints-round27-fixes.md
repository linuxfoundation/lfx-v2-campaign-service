# 2026-09-19 monitor account endpoints — round 27 review fixes

**Docs** — The Claude-Opus fallback trio, run against the combined round
24+25+26 diff, found round 26's `FetchFailed` contract-widening sweep
(item 6) had missed three more texts describing the same field:

- `internal/platform/googleads/monitor.go`'s `AccountCampaignRow.FetchFailed`
  doc comment — the platform-layer type, distinct from
  `model.AccountCampaignMetrics.FetchFailed` (the domain-layer type item 6
  actually fixed) — still cited only the round-19 metrics-parse cause.
- `internal/service/rules/monitor_shared.go`'s `fetchFailedRow` doc comment
  still said a `FetchFailed` row's numeric fields are always left at zero,
  cited a GAQL metrics-parse line number that item 4's budget-check
  insertion had since moved, and scoped the helper to
  `m.FetchFailed == true` even though `monitor_reddit.go`'s empty-`StartDate`
  branch (round 23) reuses the same builder for a row whose metrics are real
  but whose flight window is unknown.
- `internal/service/connection_monitor.go`'s `monitorTotalsFallback` doc
  comment still asserted a `FetchFailed` row always contributes zero-value
  numeric fields to the account-totals sum, which is false for a Google
  row whose budget alone was unparseable.

All three are now widened to describe both causes (metrics-fetch failure vs.
Google's budget-only-failure case), matching the wording already applied to
`model.AccountCampaignMetrics.FetchFailed`, `design/connection.go`, and
`docs/api-catalog.md` in round 26.

Also documented, as a comment only (no behavior change): the same review
flagged that `monitor_google.go` excludes a budget-only-failed row from
*every* action item, not just the budget-dependent ones — asymmetric with
`monitor_reddit.go`'s empty-`StartDate` branch, which still runs its
metrics-only action items. This is a scope judgment, not a defect the code
needs to change: most of `googleActionItems`' rules could in principle still
fire on a row whose delivery data is real and only its budget is untrusted,
but there is no evidence yet that a real budget-parse failure has ever
coincided with an actionable delivery issue on the same row, so partially
evaluating the row is deferred rather than done speculatively. The reasoning
is now recorded in `monitor_google.go`'s own comment so a future reader does
not read the asymmetry as an oversight.

One reviewer finding was not acted on: a pre-existing, repo-wide convention
of naming a human reviewer by GitHub handle in `docs/reviews/knowledge-base`-
adjacent `docs/knowledge/log/` prose (present in a dozen files as of this
range's base commit, including this feature's own round-24 log entry) was
flagged as a privacy-policy violation. It predates this range, and fixing it
in one file while leaving a dozen others unchanged would not resolve the
underlying convention question — left for a separate, deliberate decision
rather than a side effect of this fix cycle.

Verification: `gofmt -l .`, `go build ./...`, `go vet ./...`, and
`go test ./internal/platform/googleads/... ./internal/service/... ./internal/service/rules/... -count=1`
all clean/passing; `go run ./cmd/okfvalidate ./docs/knowledge` reports the
bundle conformant.
