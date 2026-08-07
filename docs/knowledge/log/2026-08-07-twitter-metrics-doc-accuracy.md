# 2026-08-07 — LFXV2-2996 X Ads metrics documentation accuracy

**Update** — Four documentation claims contradicted the shipped code, and one
catalog row pointed at a support statement that did not exist.

`twitterMetricsWindow` maps `yesterday` as well as `today` and `last_7_days`, but
`docs/knowledge/code/internal-dispatch.md` listed `yesterday` among the REJECTED
windows and `defaultMetricsWindowFor`'s comment in `internal/service/brief.go`
named only two mapped windows. Both now match the allow-list.

`dateRangeForWindow` returns the last INCLUDED day; `GetCampaignMetrics` adds the
exclusive day when it builds `end_time`. The function's own comment claimed the
increment happened inside it, which would lead a refactor to add the day twice and
silently widen every window. The comment now states where the increment lives and
why it must stay in one place.

`docs/knowledge/code/internal-platform-twitter.md` cited
`internal/platform/googleads/metrics.go` and `internal/platform/linkedin/metrics.go`
as precedent for the disclosed-assumption convention. Neither file exists on this
branch — they arrive with their own PRs — so the citation published evidence that
could not be checked. The disclosed assumption is kept; the references are removed.

`docs/api-catalog.md` ended the metrics row with "Support is per-platform (see
below)", but the section below only said adapters arrive in separate PRs. It now
carries a table recording X's three supported windows, so an API consumer can
discover the constraint without reading the dispatcher.
