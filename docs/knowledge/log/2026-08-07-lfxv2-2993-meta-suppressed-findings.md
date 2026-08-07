# 2026-08-07 — Meta metrics: second log-scrub branch, window coverage, doc corrections

**Update** — Closed the suppressed Copilot findings on PR #72 (`internal/service/brief.go`,
`internal/platform/meta/metrics_test.go`, `internal/dispatch/meta_test.go`,
`docs/api-catalog.md`, and two earlier log fragments on this branch).

`GetCampaignMetrics` had two failure-path logs and only one scrubbed. The
`ErrMetricsWindowUnsupported` branch still passed `merr` straight to `slog`. That branch is not
less exposed than the generic one: an adapter is free to wrap the sentinel around upstream text,
and some platforms echo the offending request value into their own "unsupported date range"
message, so it can carry raw response bytes exactly as the default branch's error can. Both
branches now render through `safeErrSummary`. **Scrubbing one error path and not its sibling
leaves the path open** — the question to ask is not "is this branch attacker-reachable" but "can
any implementation behind this interface put upstream bytes in this error", and for a
platform-agnostic funnel the answer is always yes.

The window translation is now pinned end to end. `metaMetricsWindow` (dispatch) and
`datePresetFor` (platform client) are two hops of one-liners, and a wrong entry in either is
invisible: swapping `last_7d` for `last_14d` compiles, passes the unsupported-window guard — it
IS a valid preset, just the wrong one — and silently reports a different reporting period than
the caller asked for. `TestMeta_ReadMetrics_EveryWindowReachesTheRightDatePreset` drives all
seven windows through the dispatcher against an `httptest` server and asserts the literal
`date_preset` on the wire, which covers both hops at once (a client-level test would still pass
if the dispatcher mapped the window to the wrong `meta.MetricsWindow`).
`TestDatePresetFor_CoversEveryWindow` lives in package `meta`, where the unexported map is
reachable, and fails when a declared window has no entry or the two lists drift apart. Both were
revert-verified: the value sabotage names the wrong preset, the size sabotage names the missing
window.

Two documentation corrections, both of the same kind — **a fragment describing an unmerged PR as
though it were the tree**. The Meta metrics fragment claimed this work "mirrors the Google Ads
GA-5 pattern exactly" and that the endpoint now works for Meta "same as Google Ads", but there is
no `GoogleAdsDispatcher.ReadMetrics` on `main` and the Google Ads concept file records metrics
reads as a later slice. GA-5 exists as an open PR, which a reader of the fragment cannot know.
The log-scrub fragment claimed both failure logs used `safeErrSummary` when only one did. Both
now say what is actually in the tree, each with a note on what the original claimed, since these
fragments are the branch's own unmerged drafts rather than landed history.

`docs/api-catalog.md` gained a Meta row in the per-platform metrics-support table (all seven
windows; the Insights `date_preset` vocabulary covers the shared set exactly, so the mapping is a
pure rename rather than a subset like X Ads'), and the surrounding wording now states plainly
that a platform absent from that table has no adapter wired and returns 400.

Concept update: `docs/knowledge/code/internal-service.md` now records the `safeErrSummary`
contract on the metrics failure paths — what it strips (non-graphic runes → U+FFFD, so the
substitution stays visible and newlines cannot forge records in a line-oriented sink), the
rune-counted 200-rune cap, why the scrub is scoped to the log call rather than to
`*meta.APIError.Error()`, and why both branches scrub.
