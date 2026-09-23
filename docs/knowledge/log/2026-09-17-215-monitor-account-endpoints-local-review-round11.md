# 2026-09-17 monitor account endpoints — local review round 11

**Fix** — An eleventh local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
three Important issues from `general`; `repo_code` and `repo_learnings` were
both clean.

1. `internal/service/connection_monitor.go`'s `monitorTotalsFallback` call
   decided `DerivedFromRows` from `platform == model.ProviderRedditAds`
   instead of from the actual failure signal already in scope
   (`terr != nil` from `orch.ReadAccountTotals`). Today the two happen to
   coincide — Reddit is the only platform with an `AccountTotalsReader`, so
   it is the only one that can reach this branch via an actual failure — but
   a second platform later implementing that interface would silently
   report `derived_from_rows: false` on a failed totals call, which is
   exactly the mis-signal the field exists to prevent, with no compiler
   warning. Fixed by capturing `totalsReadFailed := terr != nil` before the
   `!ok` fallback path resets `ok`, and passing that instead of the provider
   comparison.
2. `design/connection.go`'s `derived_from_rows` attribute description and
   `model.AccountMonitorTotals.DerivedFromRows`'s doc comment both said the
   field goes true when Reddit's totals call "failed or is unsupported" —
   but "unsupported" (no `AccountTotalsReader` at all) is the contractual,
   non-derived case for every platform, Reddit included were it ever to lose
   the capability; only an actual failed read is a stand-in. Narrowed both
   docs to "actually failed", matching fix 1's corrected predicate.
3. `internal/platform/meta/client.go`'s `timeNow` field comment and
   `WithClock`'s doc comment still described the clock as backing 429 backoff
   only, but `fetchAccountCampaignInsights` (added for the account-monitor
   endpoint) now also reads it to build the production `time_range` window
   for every insights query. Reworded both to name both consumers, so a
   future reader changing `WithClock`'s semantics on the strength of the old
   comment doesn't silently move a live reporting window.

No new correctness bugs found beyond item 1 (the provider-keyed
`DerivedFromRows` predicate); items 2 and 3 were documentation-currency
fixes. `design/connection.go`'s change required `goa gen` + the kodata
OpenAPI copy step; the resulting `gen/**` diff is limited to the one
attribute description string.
