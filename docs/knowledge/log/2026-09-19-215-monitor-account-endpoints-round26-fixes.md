# 2026-09-19 monitor account endpoints — round 26 review fixes

**Docs** — The repo-code local reviewer, run against round 24+25's combined
diff, found that round 24's Google Ads budget fix (see round-24's log entry,
item 1) had made `FetchFailed`'s published contract inaccurate rather than
the code itself wrong: `internal/domain/model/monitor.go`'s doc comment,
`design/connection.go`'s `fetch_failed` attribute description, and
`docs/api-catalog.md`'s account-monitor row all said `FetchFailed=true`
implies the row's numeric metrics are zero-value placeholders — true for a
metrics-fetch failure, but not for a Google Ads row whose budget alone was
unparseable (`TestListAccountCampaigns_MalformedBudget_MarksFetchFailed`
pins exactly this: `FetchFailed=true` alongside real non-zero
impressions/clicks/spend). The code's behavior — excluding the row from
pacing/action-item evaluation regardless — was already correct and
deliberate; only the description was stale. All three texts, plus a stale
comment in `internal/service/rules/monitor_google.go` citing only the
round-19 metrics-parse cause, are now widened to describe both causes.
`design/connection.go`'s change was carried through `goa gen` (`gen/http/**`,
`gen/lfx_v2_campaign_service_connections/**`, including the openapi spec
copies under `cmd/campaign-service/kodata/gen/http/`).

Also added a pinning test the general reviewer flagged as missing: round
25's fix (checking `budgetOK` on every GAQL row, not just a campaign id's
first-sighting row) had no test that actually exercised a malformed budget
on a *later* row, since the existing single-row fixture puts the malformed
value on the only row either way.
`TestListAccountCampaigns_MalformedBudgetOnLaterRow_MarksFetchFailed` uses a
two-row fixture for one campaign id (parseable budget first, malformed
second) and asserts `FetchFailed=true` with both rows' metrics accumulated.

Verification: `gofmt -l .`, `go build ./...`, `go vet ./...`, and
`go test ./... -count=1` (full repo suite) all clean/passing;
`go run ./cmd/okfvalidate ./docs/knowledge` reports the bundle conformant.
