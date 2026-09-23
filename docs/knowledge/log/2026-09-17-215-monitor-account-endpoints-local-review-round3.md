# 2026-09-17 monitor account endpoints — local review round 3

**Fix** — A third local pre-PR review round (general, repo_code, and
repo_learnings reviewers) on the account-monitor-endpoints branch converged on
one shared finding and surfaced two more:

1. All three reviewers flagged the same truthfulness gap left by round 2's
   fix: each rule engine's `FetchFailed` branch sets `PacingUnknown = true`
   but leaves `PacingLabel` at its `MonitorPacingNormal` zero value, and
   nothing said so anywhere a reader would look first — `pacing_label`'s own
   design-DSL doc comment (`design/connection.go`), the round-2 log entry's
   wording ("stays at the placeholder" read as ambiguous), the rule-engine
   comment in `monitor_google.go`, and `docs/api-catalog.md`'s endpoint row
   all described the pair inconsistently or not at all. Fixed by explicitly
   documenting the placeholder-not-fabrication reading everywhere it was
   missing, mirroring `pacing_pct`'s existing "meaningless when
   pacing_unknown is true" convention: `design/connection.go`'s
   `pacing_label` attribute description, `monitor_google.go`'s `FetchFailed`
   comment (the one the other three rule engines point back to), and
   `docs/api-catalog.md`'s FetchFailed sentence. Also added an explicit
   `PacingLabel` assertion to all four `TestEvaluate*Monitor_SkipsFetchFailedRows`
   tests, which previously asserted `FetchFailed`/`PacingUnknown` but let a
   stale or fabricated label pass unnoticed.
2. `repo_code` flagged that `docs/api-catalog.md`'s account-monitor row
   documented no `account_id` shape constraint and no malformed-id 400 case,
   even though [Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md)
   documents both in detail. Added a paragraph covering the per-provider
   shape constraints (LinkedIn/Meta `Pattern` at the design layer; Google/
   Reddit `MinLength`/`MaxLength` plus dispatcher-level
   `domain.ErrAccountIDMalformed`) and the malformed-`account_id` 400 case to
   the status-mapping enumeration.
3. `general` flagged that the architecture doc's stated rationale for why
   Google/Reddit lack a design-layer `Pattern` ("neither has an established
   Pattern convention... inventing one would be an unreviewed shape
   decision") was factually wrong — both `googleads.customerIDRE` and
   `reddit.accountIDRe` already existed and were already used elsewhere in
   this same commit range. Corrected to the true rationale: avoiding two
   independently-drifting definitions of "valid account id," one in the Goa
   DSL and one in the platform client — the platform client stays the single
   owner of that shape.
4. `general` also flagged (Important, confidence 85) that Reddit's
   `account_id` shape check ran *after* `resolveMonitorClient`, which
   resolves and decrypts the stored credential, unlike Google's, which
   validates before any credential resolution — an unauthenticated
   malformed-id caller should never cost a credential decrypt. Fixed with a
   new exported `reddit.ValidateAccountID` (mirroring
   `googleads.ValidateCustomerID`), called in
   `internal/dispatch/reddit.go`'s `ListAccountCampaignMetrics` before
   `resolveMonitorClient`.
5. `general` also flagged (finding #4) that LinkedIn's and Meta's
   dispatcher-level `ListAccountCampaignMetrics` had no shape check of their
   own — they relied entirely on Goa's design-layer `Pattern`, leaving a
   residual unsentineled-503 gap for any non-HTTP caller that bypasses the
   HTTP boundary. Fixed with new exported `linkedin.ValidateAccountID` /
   `meta.ValidateAccountID` functions (each reusing that package's existing
   `accountIDRE`), called in their respective dispatchers before credential
   resolution, wrapping a failure in `domain.ErrAccountIDMalformed` — the
   same defense-in-depth pattern Google/Reddit already had.

**Repo-convention corrections** — `repo_code` also caught two violations of
this repo's "never edit another entry's file" rule from earlier in this same
review cycle: a one-character typo "fix" applied to the pre-existing,
already-committed
[2026-09-17-monitor-account-endpoints.md](2026-09-17-monitor-account-endpoints.md)
entry (reverted — the original typo text is restored), and two log fragments
introduced on this branch that lacked any ticket-equivalent identifier in
their filenames (renamed to carry PR #215's number, the confirmed
ticket-equivalent for this work, since no JIRA/LFXV2 ticket exists).

Deliberately deferred to follow-up tickets, to keep this round's diff
bounded: Reddit's `accountTotals` independent-upstream-call-vs-row-sum
ambiguity (a `derived_from_rows` design field would be needed); Meta's
`Pattern` vs `normalizeMetaAccountID` mismatch beyond the documentation note
already added; a stale "single-page" comment in
`internal/domain/model/monitor.go`; a loose citation in
`internal/platform/linkedin/monitor.go`.
