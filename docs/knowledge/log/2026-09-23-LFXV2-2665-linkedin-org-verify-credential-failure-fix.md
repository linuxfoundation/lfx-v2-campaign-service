# 2026-09-23 LinkedIn org-verification: don't fold credential failures into inconclusive

**Fix** — PR #223 review (Copilot + Cursor Bugbot, independently, same finding) flagged that
`internal/platform/linkedin/accounts.go`'s `VerifyAccountOrgReference` wrapped EVERY
`ListAdAccounts` failure in `ErrOrgVerificationInconclusive`, including credential and
application-authorization failures (a GET 401 becomes `ErrCredentialsExpired` two layers down).
`internal/service/connection.go`'s `TestLinkedinAds` checks
`errors.Is(verr, linkedin.ErrOrgVerificationInconclusive)` FIRST, before any other
classification, so an expired or revoked LinkedIn credential was reported as `OK: true` with an
inconclusive advisory message instead of failing the connection test — the credential baseline
`testConn` runs earlier only checks that encrypted credential bytes exist, never that they are
still valid upstream.

Fixed at the source: `VerifyAccountOrgReference` now checks the `ListAdAccounts` error against
the three permanent credential/authorization sentinels (`ErrCredentialsExpired`,
`ErrApplicationCredentialsInvalid`, `ErrTokenRequestRejected`) and returns it UNWRAPPED when it
matches, instead of wrapping it in `ErrOrgVerificationInconclusive`. Only a transport or
pagination failure — one that genuinely proves nothing about the account/org pairing — still
gets the inconclusive wrap. Downstream, `internal/dispatch/linkedin.go`'s existing
`res.systemScoped(linkedinExpiry(verr))` call already re-tags an unwrapped credential sentinel
into `domain.ErrConnectionNotUsable`, so no change was needed there or in `connection.go`: the
inconclusive-first check in `TestLinkedinAds` simply no longer matches a credential failure, and
the connection test now fails as it should.

Added a regression subtest to `internal/platform/linkedin/accounts_test.go`
(`TestVerifyAccountOrgReference/expired credentials fail the verification, not folded into
inconclusive`) pinning both halves: the error `errors.Is` `ErrCredentialsExpired`, and does
NOT `errors.Is` `ErrOrgVerificationInconclusive`.

Also added a dedicated `OrgReferenceVerifier` section to
[internal-dispatch.md](../code/internal-dispatch.md), mirroring the existing `CampaignAdopter`
section — the concept file for the package that physically implements
`LinkedInDispatcher.VerifyAccountOrg` had no section for this optional capability, even though
[internal-platform-linkedin.md](../code/internal-platform-linkedin.md) already called it "a
fifth optional capability" (PR #223 review, dealako).

Verified clean: `gofmt -l .`, `go vet ./...`, `go build ./...`, and
`go test ./internal/platform/linkedin/... ./internal/dispatch/... ./internal/service/...` all
pass, including the new regression subtest.
