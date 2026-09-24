# 2026-09-23 LinkedIn VerifyAccountOrg: split confirmed failures from inconclusive ones

**Fix** — Follow-up to the same-day review-trio fixes
(`2026-09-23-LFXV2-2665-linkedin-verify-account-org-review-fixes.md`). The general
reviewer flagged two behavior gaps in `VerifyAccountOrgReference`
(`internal/platform/linkedin/accounts.go`) that were pre-existing shipped
behavior, not regressions from that round, so they were deferred for
explicit sign-off before changing production connection-test semantics.
Both are now resolved:

1. **Account absent from a complete walk now fails closed.** `ListAdAccounts`
   returns every account or an error — every truncation mode (repeated
   cursor, missing `elements`/`metadata`, the page cap) already fails the
   walk before `VerifyAccountOrgReference` can reach its "not found" branch.
   Reaching that branch therefore means the walk genuinely completed without
   a match, which is a confirmable fact (this token cannot reach the
   configured account), not an inconclusive one. It previously folded into
   the same `nil` as a genuine match; it now returns an error, same as a
   confirmed org mismatch.
2. **A failed enumeration walk is no longer conflated with a confirmed
   failure.** A `ListAdAccounts` transport/credential failure, or hitting the
   20-page cap on a very large token, proves nothing about the account/org
   pairing — only that the check couldn't run. It is now wrapped in the new
   exported sentinel `linkedin.ErrOrgVerificationInconclusive`.
   `ConnectionService.TestLinkedinAds` (`internal/service/connection.go`)
   checks for that sentinel with `errors.Is` and reports `OK: true` with an
   advisory message for it, instead of folding it into `OK: false` alongside
   a real confirmed mismatch — a large or transiently flaky token no longer
   makes a healthy connection look broken.

Updated: `VerifyAccountOrgReference`'s doc comment, the `OrgReferenceVerifier`
interface doc comment (`internal/service/orchestrator.go`), the LinkedIn
`/test` row in `docs/api-catalog.md`, and the "Org/account reference
verification" and "LinkedIn org/account pairing verification" sections in
`internal-platform-linkedin.md` / `internal-service.md`. New tests: two cases
in `TestVerifyAccountOrgReference` (`accounts_test.go`) covering the
now-confirmed absent-account case and the inconclusive-walk-failure case, and
one new subtest in `TestTestLinkedinAds_UpstreamVerification`
(`connection_test.go`) covering the `OK: true`-with-advisory-message path.
