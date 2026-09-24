# 2026-09-23 LinkedIn org-verification: 403s, decrypt-error leaks, service defects

**Fix** — A second Copilot pass on PR #223 (after the credential-sentinel fix landed) found
three more real gaps in the same outcome model, all in the same family as the earlier fix:

1. **403 not caught.** `VerifyAccountOrgReference`'s sentinel check
   (`ErrCredentialsExpired`/`ErrApplicationCredentialsInvalid`/`ErrTokenRequestRejected`) missed a
   403 response from `GET /adAccounts`: LinkedIn has no dedicated sentinel for it, so it reached
   the fallback as a bare `*apiError` and was wrapped in `ErrOrgVerificationInconclusive` —
   reported as `OK: true` even though LinkedIn had definitively refused this credential
   permission to enumerate ad accounts. Fixed by adding an `errors.As(err, &aerr) &&
   aerr.StatusCode == http.StatusForbidden` check, returning the error unwrapped like the other
   three sentinels.
2. **Decrypt-error leak.** `TestLinkedinAds` (`internal/service/connection.go`) built its
   failed-test message by concatenating `verr.Error()` for every non-inconclusive error,
   including `domain.ErrCredentialDecryptionFailed` — whose wrapped chain comes from
   `domain.Encryptor` and, per the warning at `internal/dispatch/creds.go:843-868`, may include
   ciphertext or key material. Fixed by classifying this sentinel FIRST (mirroring
   `classifyDiscoveryError`'s identical arm), logging only structured fields via `slog`, and
   returning a fixed generic `*conn.InternalServerError` message with no error text.
3. **Service defect miscategorized.** `linkedinExpiry` deliberately re-tags a malformed refresh
   request as `domain.ErrServiceDefect`, but `TestLinkedinAds`'s catch-all converted it to an
   ordinary `OK: false` failed test — misattributing a defect in THIS service to the stored
   connection. Per the repo's established `ErrServiceDefect` convention
   (`internal/service/service_defect_status_test.go`), fixed by classifying it before the
   catch-all and mapping it to a typed `*conn.InternalServerError`, logging the safe
   `unusableConnectionReason(verr)` string rather than the raw error.

Also narrowed the "inconclusive" bucket description that both
[internal-platform-linkedin.md](../code/internal-platform-linkedin.md) ("Org/account reference
verification" section) and `docs/api-catalog.md`'s LinkedIn `/test` row used to state too broadly
— both now say a credential/authorization/403 failure surfacing during the enumeration walk is
NOT inconclusive, since it proves the credential cannot perform the verification at all; only a
non-authentication transport/pagination failure still gets the inconclusive wrap. The API-catalog
row also now documents the two failure classes that return a typed 500
(`ErrCredentialDecryptionFailed`, `ErrServiceDefect`) instead of failing the test.

Added regression coverage: `accounts_test.go`'s `TestVerifyAccountOrgReference/a 403 fails the
verification, not folded into inconclusive` (asserts an unwrapped `*apiError` with
`StatusCode == 403`, and that it does not `errors.Is` the inconclusive sentinel), and two new
subtests under `TestTestLinkedinAds_UpstreamVerification` in `connection_test.go`: one pinning
that a marker-bearing decrypt error never appears in the `*conn.InternalServerError.Message`, and
one pinning that `ErrServiceDefect` maps to a typed 500 rather than an `OK: false` result.

Verified clean: `gofmt -l .`, `go vet ./...`, `go build ./...`, and
`go test ./internal/platform/linkedin/... ./internal/service/... ./internal/dispatch/...` all
pass, including the four new subtests.
