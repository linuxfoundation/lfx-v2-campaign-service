# 2026-09-21 PR #215 monitor account endpoints round 32 unattributable rows

**Fix** — Fixed two "previously missed" findings surfaced in the same
Copilot review (`pullrequestreview-5265717865`) that named round 31's three
doc-only threads. These two have no corresponding GitHub inline review
thread — they exist only as narrative text in that review's "Previously
missed" section, on code unchanged since the prior review — so there is no
thread to reply-to/resolve; this entry is the only record of the fix.

1. `internal/platform/linkedin/monitor.go`'s `fetchAccountCampaignAnalyticsRaw`
   silently `continue`'d past an analytics element whose `pivotValues` was
   empty, or whose first value didn't resolve to a campaign id via
   `trailingID`. That dropped the row from the returned map entirely, with
   no id recorded anywhere. `ListAccountCampaigns` (this file, `:80-110`)
   already documents that a campaign id absent from a *successful* analytics
   response is read as a legitimate zero-activity omission — so silently
   dropping an unattributable row made it indistinguishable from that
   legitimate case, converting an unreadable upstream row into false
   no-delivery/underspending findings. Fixed by rejecting the whole
   analytics read (returning a `transportError`) when a row can't be
   attributed to a campaign id, the same way this function already rejects
   a missing `elements` field — there is no id to key a narrower
   per-campaign `FetchFailed` on.
2. `internal/platform/meta/monitor.go`'s `fetchAccountCampaignInsights` had
   the opposite gap: its parse-failure path already tracked an
   unattributable row correctly (`failed[campaignID]`, letting
   `ListAccountCampaigns` mark that one row `FetchFailed`) — but only when
   `row.CampaignID != ""`. A row with a missing/empty `campaign_id` matched
   neither that guard nor the success path meaningfully (it would have
   written into `out[""]`, an id no real campaign has), so it was silently
   invisible to `ListAccountCampaigns` — same false-zero outcome as finding
   1. Fixed by rejecting the whole insights read when a row's `campaign_id`
   is empty, before the parse-failure branch runs; the parse-failure
   branch's now-redundant `if row.CampaignID != ""` guard was simplified
   since `CampaignID` is guaranteed non-empty at that point.

Both fixes reject the entire read rather than marking a single row failed,
because — unlike the malformed-budget/`costInUsd` case this same pattern
already handles — there is no campaign id available to attribute the
failure to individually.

Verified clean after both fixes: `gofmt -l .`, `go vet ./...`,
`go build ./...`, and `go test ./...` (every package, including
`internal/platform/linkedin` and `internal/platform/meta`) all pass; no
existing test asserted the old silent-drop behavior.
