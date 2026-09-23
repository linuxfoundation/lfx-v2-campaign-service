# 2026-09-17 monitor account endpoints — local review round 12

**Fix** — A twelfth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
two Important issues from `general`; `repo_code` and `repo_learnings` were
both clean.

1. All four account-monitor dispatchers (`googleads.go`, `meta.go`,
   `linkedin.go`, `reddit.go`) already re-validate `account_id`'s shape as
   defense-in-depth for a non-HTTP caller that bypasses the Goa design
   layer's own `Pattern` check, but left `days` unchecked on that identical
   surface even though the design layer's `Minimum(7)/Maximum(90)` is just
   as bypassable. `internal/platform/meta/monitor.go` and
   `internal/platform/linkedin/monitor.go` even document this as "a
   deliberate asymmetry" — but an unchecked `days` of 0 or negative inverts
   the `[start, end]` window each dispatcher computes from it, producing the
   same opaque 503 the `account_id` check exists to avoid. Fixed by adding a
   new domain sentinel `ErrMonitorDaysInvalid` (classified to 400 in
   `classifyDiscoveryError`, alongside `ErrAccountIDMalformed`) and a shared
   `dispatch.validateMonitorDays` helper (`internal/dispatch/monitor_validation.go`,
   package-shared since all four dispatchers live in `package dispatch`),
   wired into each dispatcher's `ListAccountCampaignMetrics` immediately
   after its existing `Validate*AccountID` call.
2. `internal/platform/linkedin/targeting.go`'s `linkedin.ValidateAccountID`
   and `internal/platform/meta/client.go`'s `meta.ValidateAccountID` both
   returned a bare `fmt.Errorf(...)` with no sentinel, unlike
   `googleads.ErrNotACustomerID` and `reddit.ErrInvalidAccountID` — so
   neither could be `errors.Is`-classified, `internal/dispatch/reddit.go`'s
   documented defense-in-depth remap (a shape error escaping the platform
   client mid-request gets remapped to `domain.ErrAccountIDMalformed`
   instead of falling through to a generic 503) had no equivalent on
   LinkedIn or Meta, and `internal/platform/linkedin/monitor_test.go`'s
   existing test was forced into a brittle
   `strings.Contains(err.Error(), "invalid LinkedIn ad account id")`
   assertion. Fixed by adding `linkedin.ErrInvalidAccountID` and
   `meta.ErrInvalidAccountID` sentinels (wrapped with `%w` at each
   `Validate*AccountID`), adding the matching remap to
   `internal/dispatch/linkedin.go` and `internal/dispatch/meta.go`'s
   `ListAccountCampaignMetrics` (mirroring reddit.go's), and switching the
   LinkedIn test assertion to `errors.Is(err, ErrInvalidAccountID)`.

Both fixes extend an existing pattern already proven correct for
`account_id`/Reddit to the remaining three platforms and to `days`, rather
than introducing a new one.
