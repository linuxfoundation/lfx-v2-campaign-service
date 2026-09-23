# 2026-09-17 monitor account endpoints — local review round 16

**Fix** — A sixteenth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
one Critical issue and one Important issue from `general`; `repo_code` and
`repo_learnings` were both clean.

1. **Critical** — Round 15 documented, but did not fix, the credential-scope
   gap on `ListAccountCampaignMetrics`: a project with no Google/LinkedIn/Meta
   connection of its own falls back to the shared LF system credential and can
   read another project's spend/budget/campaign data for any account id that
   credential reaches. Round 16 escalated this to Critical because the four
   monitor endpoints are externally routed (this branch's
   `httproute.yaml`/`ruleset.yaml` chart changes), unlike the internal-only
   surfaces the gap was previously weighed against.

   Fixed identically across all three affected dispatchers by adding a new
   "owned discovery" resolver per platform — account-agnostic like the
   existing discovery resolver, but refusing the LF system fallback the way
   `resolveOwned` (`internal/dispatch/creds.go`) already does for the
   adoption flow — and switching `ListAccountCampaignMetrics` to call it:
   `resolveOwnedGoogleAdsDiscoveryClient` (`internal/dispatch/googleads.go`),
   `resolveLinkedInOwnedDiscoveryCredentials` (`internal/dispatch/linkedin.go`),
   `resolveOwnedMetaDiscoveryClient` (`internal/dispatch/meta.go`). No new
   domain sentinel was needed: `classifyDiscoveryError`
   (`internal/service/connection.go`) already maps `domain.ErrNotFound` to a
   404 "no connection configured for this project", which is exactly the
   right response once the fallback is refused. Reddit needs no change — its
   dispatcher already scopes `accountID` to the resolved connection's own
   single account rather than building an account-agnostic client.

   Added `TestGoogleAds_ListAccountCampaignMetrics_RefusesSystemFallback`,
   `TestLinkedIn_ListAccountCampaignMetrics_RefusesSystemFallback`, and
   `TestMeta_ListAccountCampaignMetrics_RefusesSystemFallback`, each using
   `scopedConnReader` (`internal/dispatch/creds_test.go`) configured with a
   valid connection only under `model.SystemProjectID` and none under the
   test's own project id — proving the fallback is actually refused
   (`errors.Is(err, domain.ErrNotFound)`), not merely that some unrelated
   error surfaces.

2. **Important** — Fixing the Meta `time_range` window (round-15's third
   quirk, `1061e662`) silently changed which calendar days a default-window
   request covers: Meta's own `last_30d` preset excludes today, but the
   explicit range `{since: today-(days-1), until: today}` includes it, so a
   `days=30` request now returns 29 full days plus one partial trailing day
   instead of 30 full days. Resolved as a deliberate, documented divergence
   rather than a defect: kept because it matches the days-1-ending-today
   convention Google/Reddit's monitor dispatchers already use, so all four
   platforms answer "last N days" identically. Documented in
   `fetchAccountCampaignInsights`'s doc comment
   (`internal/platform/meta/monitor.go`) and in the "Known-verbatim-ported
   quirks" section of
   `docs/knowledge/architecture/account-monitor-endpoints.md`.
