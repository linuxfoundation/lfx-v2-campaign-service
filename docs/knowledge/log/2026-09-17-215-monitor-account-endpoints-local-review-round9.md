# 2026-09-17 monitor account endpoints — local review round 9

**Fix** — A ninth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
one issue all three converged on plus three further Important issues from
`general` and one Should-fix issue from `repo_code`:

1. (all three) Round 7's fix added a `Pattern` to Google Ads' and Reddit's
   `account_id` design attributes (`design/connection.go`) and dropped their
   `MinLength(1)`, but roughly nine comment/doc sites elsewhere in the repo
   still asserted the OLD state — that Google Ads and Reddit have no
   design-layer `Pattern`, "a deliberate ownership choice, not a gap."
   Updated every stale site to describe the current state instead:
   `docs/api-catalog.md`, `docs/knowledge/architecture/account-monitor-endpoints.md`,
   `internal/domain/errors.go` (`ErrAccountIDMalformed`'s doc comment),
   `internal/dispatch/googleads.go` and its test,
   `internal/dispatch/reddit_test.go`, and
   `internal/service/connection_monitor_test.go`. The architecture doc keeps
   the old reasoning as history (`account_monitor_endpoints.md`'s narrative
   already covers Google/Reddit's original `MinLength`-only decision), with a
   new paragraph describing the later reversal and the drift test that now
   guards it.
2. (general) The drift test
   (`internal/apivalidation/monitor_account_id_drift_test.go`) missed a real
   divergence: `reddit.ValidateAccountID` and `meta.ValidateAccountID` both
   called `strings.TrimSpace` before matching their regex, while the design
   `Pattern`s never trim — so e.g. `" t2_abc"` was accepted by the platform
   validator but rejected by the design `Pattern`, the exact kind of silent
   separation the drift test exists to catch. It also only covered Google Ads
   and Reddit, leaving LinkedIn and Meta unchecked. Fixed by dropping
   `TrimSpace` from both `reddit.ValidateAccountID`
   (`internal/platform/reddit/monitor.go`) and `meta.ValidateAccountID`
   (`internal/platform/meta/client.go`) — the smaller change, since neither
   `googleads.ValidateCustomerID` nor `linkedin.ValidateAccountID` ever
   trimmed, so this makes all four validators uniform. Extended the drift
   test with LinkedIn and Meta subtests and whitespace cases for all four.
3. (general) `docs/knowledge/code/internal-service.md`'s description of
   `classifyDiscoveryError` said "Five outcomes are distinguished
   deliberately," a fixed count that predates this round's new
   `domain.ErrAccountIDMalformed` arm and the folding of
   `domain.ErrAccountMetricsUnsupported` into the `ErrAccountsUnsupported`
   arm — neither reflected in the doc. Replaced the fixed count with
   property-based language, added the missing `ErrAccountIDMalformed → 400`
   bullet, and noted `ErrAccountMetricsUnsupported`'s folding in the first
   bullet.
4. (general) `fetchFailedRow` (`internal/service/rules/monitor_google.go`)
   is genuinely cross-platform — called from all four
   `Evaluate*Monitor` functions, and its own doc comment already said so —
   but lived in the Google-specific file, violating this package's strict
   per-platform (`monitor_google.go` … `monitor_reddit.go`) vs. shared
   (`pacing.go`/`actions.go`/`window.go`) file-layout convention. Moved it,
   unchanged, to a new `monitor_shared.go`.
5. (repo_code) The drift test hand-copied each design `Pattern` as a
   `regexp.MustCompile` literal instead of driving the generated decoder,
   conceding in its own header comment that it "must be kept in sync by
   hand." Rewrote it to route a real request through the actual generated
   `DecodeMonitor*AccountRequest` decoders via a `goahttp.Muxer`, the same
   pattern `internal/service/brief_test.go`'s
   `TestFindBriefDecoder_RejectsEmptySlugButNotLongOnes` already uses — so a
   regenerated `Pattern` is picked up automatically instead of silently going
   stale relative to the copied literal.

No new correctness bugs found this round; all five items were
documentation-currency or test-robustness fixes, not behavior changes.
