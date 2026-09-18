# 2026-09-17 monitor account endpoints — local review round 7

**Fix** — A seventh local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
three Important issues from `general`, one Important issue from
`repo_learnings`, and none from `repo_code`:

1. `internal/service/connection_monitor.go`'s totals-fallback branch caught
   `domain.ErrConnectionNotUsable` to avoid leaking credential-decryption
   detail into logs, but not the two sibling sentinels that can also reach
   this path without that wrapper —
   `domain.ErrCredentialDecryptionFailed` (set at
   `internal/dispatch/creds.go:876`, before
   `resolveRedditClientWithCreds`'s `systemScoped` wrapper ever registers)
   and `domain.ErrServiceDefect`. Widened the branch to also match those two
   via `errors.Is`, routing them through the same fixed-vocabulary
   `unusableConnectionReason` helper, which already falls through to
   `"unclassified"` for both (no case exists for either — confirmed safe
   before making the change).
2. `model.AccountMonitorTotals` carried no way to distinguish Reddit's
   platform-native account totals from the row-summed fallback
   (`monitorTotalsFallback`) — both use identical field names, so a caller
   couldn't tell a real account-wide figure from a derived stand-in. Added
   `derived_from_rows` to the design type and `DerivedFromRows` to the domain
   struct and generated response, wired through
   `toConnAccountMonitorTotals`.

   Fixing this correctly required catching a self-introduced bug before it
   shipped: an early draft set `DerivedFromRows: true` unconditionally inside
   `monitorTotalsFallback`, on the mistaken assumption that every caller of
   that function reaches it only on failure. That's false — Reddit is the
   *only* platform whose dispatcher implements `AccountTotalsReader`
   (`internal/dispatch/reddit.go:503`); every other platform's
   `Orchestrator.ReadAccountTotals` call returns `ok=false` with a nil error
   on *every* request, not on failure, because it has no such capability.
   For those platforms `monitorTotalsFallback` is the normal, contractual
   totals computation (`model.AccountMonitorTotals`' own doc comment: their
   totals ARE a row sum by design), not a degraded stand-in. Marking their
   totals "derived" would have been backwards. Fixed by moving the decision
   to `monitorAccount`'s one call site, which sets
   `DerivedFromRows: platform == model.ProviderRedditAds` — the only
   condition under which reaching this function ever represents a real
   substitution for an unavailable platform-native number.
3. `Orchestrator.ReadAccountTotals`'s nil-result contract-violation error
   (a broken `AccountTotalsReader` adapter returning `(nil, nil)`) was an
   unsentineled `fmt.Errorf`, identical in shape to seven other
   "(nil, nil) is a contract violation" sites elsewhere in
   `orchestrator.go` — but unlike those, this one's sole caller folds it
   into the routine WARN-level "account totals read failed; serving the
   row-summed fallback" log line, indistinguishable from an ordinary
   upstream timeout or 5xx. Added a new unexported sentinel,
   `errAccountTotalsContractViolation`, wrapped into that error path only
   (not the other seven — those still deliberately follow the repo's
   established unsentineled convention), and had `connection_monitor.go`
   check for it first via `errors.Is` and log it at ERROR instead.

4. (repo_learnings) `design/connection.go`'s Google Ads and Reddit
   `monitor-*-ads-account` methods constrained `account_id` with only
   `MinLength(1)`/`MaxLength(64)`, unlike LinkedIn's and Meta's sibling
   methods, which already carry a `Pattern`. Both platforms' dispatchers
   independently re-validate the same shape at runtime
   (`googleads.customerIDRE`/`ValidateCustomerID`,
   `reddit.accountIDRe`/`ValidateAccountID`), so the design layer was
   looser than the runtime it fronts — a caller-supplied id could pass Goa
   and still be rejected downstream, turning what should be a 400 into a
   500-shaped platform failure. Added `Pattern(`^[0-9]+$`)` to Google Ads'
   attribute and `Pattern(`^[A-Za-z0-9_]+$`)` to Reddit's, mirroring the
   platform regexes exactly, then ran `make apigen` to regenerate `gen/**`
   and the kodata OpenAPI copies. Also added a drift test,
   `internal/apivalidation/monitor_account_id_drift_test.go`, asserting a
   table of ids classifies identically under the design Pattern and the
   platform's exported validator for both platforms, so the two cannot
   silently separate again. Along the way, fixed a now-stale comment on
   `googleads.ErrNotACustomerID` that said this account-monitor method had
   "no design-layer Pattern to reuse" — no longer true after this fix.

No new correctness bugs found this round beyond the self-caught
`DerivedFromRows` mistake in finding #2's draft, which was corrected before
verification rather than shipped.
