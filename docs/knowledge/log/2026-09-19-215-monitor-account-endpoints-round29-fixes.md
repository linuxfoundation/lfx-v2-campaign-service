# 2026-09-19 monitor account endpoints — round 29 review fixes

**Fix** — The Claude-Opus fallback trio's round-29 rerun (pinned to the
original base) came back with `repo_code` and `repo_learnings` both fully
clean (0 findings) and `general` reporting one Important finding plus a
privacy concern the tree-only redaction in round 28 had not fully resolved.

1. **Important.** `internal/platform/googleads/monitor.go`'s
   `ListAccountCampaigns` only ever assigned `BudgetDailyUSD` on a campaign
   id's first-sighting GAQL row. If that first row's `amount_micros` failed
   to parse but a *later* row for the same campaign carried a parseable
   value, the good value was silently discarded and `BudgetDailyUSD` stayed
   stuck at `0` forever — the mirror image of round 25's fix, which handled
   a malformed value on a later row but never handled recovery the other
   way around. Not a trust bug (`FetchFailed=true` was already set either
   way, so no caller reads the `0` as a real budget), but the row reported
   an unnecessarily fabricated-looking `$0` where a real number was
   available. Now tracks, per campaign id, whether the stored budget has
   ever come from a successful parse, and upgrades `BudgetDailyUSD` the
   first time a later row's budget parses while the earlier one hadn't.
   `FetchFailed` is unaffected — it still records that at least one row's
   budget was inconsistent/malformed. New test
   `TestListAccountCampaigns_MalformedBudgetOnFirstRow_LaterRowRecovers`
   pins it, mirroring
   `TestListAccountCampaigns_MalformedBudgetOnLaterRow_MarksFetchFailed`.
2. **Privacy.** The general reviewer pointed out that redacting the
   reviewer handle from the *tip tree* of
   `docs/knowledge/log/2026-09-19-215-monitor-account-endpoints-round24-fixes.md`
   (round 28) does not remove it from the *history* that would be pushed:
   the original commit `a5998d5e` still carried the handle twice in its
   patch, and once in its own commit message ("... plus a human reviewer's
   `fetchFailedRow`-duplication nit"). Since this branch has never been
   pushed, fixing it was safe as a local history rewrite rather than a new
   commit on top: an interactive rebase edited `a5998d5e` in place (message
   reworded, both doc lines given the same "a human reviewer" phrasing
   round 28 already used forward), and the later commits (`3bd6e51a`
   through this one) replayed cleanly on top with unchanged content. The
   branch's commit SHAs all changed as a result; nothing else about their
   content did. Verified with `git log -p --format=%B c1a8f099..HEAD |
   grep -i <the handle>` (checks commit message *bodies*, not just subject
   lines, which `--oneline` would miss) and `git grep -i <the handle> HEAD`
   (checks the tree) — the handle appears nowhere in this feature's range;
   the unrelated hits elsewhere in repo history (other PRs, outside this
   range) are the dozen-pre-existing-files question already deferred as a
   separate maintainer decision.

Verification: `gofmt -l .`, `go build ./...`, `go vet ./...`, and
`go test ./internal/platform/googleads/... -race -count=1` clean/passing;
`go run ./cmd/okfvalidate ./docs/knowledge` reports the bundle conformant.
