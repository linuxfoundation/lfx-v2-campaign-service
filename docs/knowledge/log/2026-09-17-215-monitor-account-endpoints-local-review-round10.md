# 2026-09-17 monitor account endpoints — local review round 10

**Fix** — A tenth local pre-PR review round, still pinned to the branch's
original base, found three Important issues from `general` and two
Should-fix issues from `repo_code`, converging on two of the same sites
(`repo_learnings` was clean):

1. (general, repo_code — convergent) `meta.Client.ListAccountCampaigns`
   (`internal/platform/meta/monitor.go`) trimmed `accountID` with
   `strings.TrimSpace` **before** calling `meta.ValidateAccountID`, so a
   whitespace-padded id such as `" act_123 "` passed this client's own check
   even though the design-layer `Pattern` (and a direct,
   untrimmed `meta.ValidateAccountID(accountID)` call) both reject it — a
   live divergence, not just dead code. Fixed by validating and using the raw
   `accountID` throughout, matching Reddit's already-correct ordering.
2. (general) Round 9's fix removed `TrimSpace` from inside
   `reddit.ValidateAccountID` itself, which made the
   `id := strings.TrimSpace(accountID)` in
   `reddit.Client.ListAccountCampaigns` and `FetchAccountTotals`
   (`internal/platform/reddit/monitor.go`) dead code — validation already
   guarantees no whitespace survives, so the trim could never change
   anything downstream. Removed both, using `accountID` directly at every
   call site (including a second, easy-to-miss `id` usage deeper in
   `ListAccountCampaigns`'s per-campaign report-fetch loop).
3. (repo_code) `googleads/client_test.go`'s `TestValidateCustomerID` doc
   comment still said the account-monitor dispatcher has "no design-layer
   `Pattern` to reuse" — the exact stale claim round 9's fix was supposed to
   have swept, but round 9 couldn't locate this particular site by grep.
   Reworded to describe the current state: the dispatcher re-checks the same
   shape the design `Pattern` already enforces, as defense-in-depth for a
   non-HTTP caller that bypasses Goa.
4. (general) The drift test's
   (`internal/apivalidation/monitor_account_id_drift_test.go`) header comment
   claimed the design-layer `Pattern` and the four platform `Validate*`
   helpers agree on the account id's full shape, but the design attributes
   also carry `MaxLength(64)` while none of the four platform validators
   bounds length at all — the two layers already disagree for a 65+
   character otherwise-valid id. Not a live defect (the design layer is the
   stricter outer gate, so an HTTP caller is refused before a non-HTTP
   caller's dispatcher-level check ever runs), but the comment overclaimed
   parity. Narrowed the language to "charset" throughout and added a
   paragraph explaining the deliberate charset-only scope.

No new correctness bugs found beyond item 1 (Meta's trim-before-validate);
the rest were documentation-currency or dead-code cleanups.
