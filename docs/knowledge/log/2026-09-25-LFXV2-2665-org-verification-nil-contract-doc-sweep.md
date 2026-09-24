# 2026-09-25 The org-verification `nil` contract, stated the same way everywhere

**Docs** — A fifth Copilot pass on PR #223 found four places still describing the OLD `nil`
contract of the LinkedIn org cross-check. The behaviour change that invalidated them landed the
same day (see
[2026-09-25-LFXV2-2665-malformed-org-id-fails-the-connection-test.md](2026-09-25-LFXV2-2665-malformed-org-id-fails-the-connection-test.md)),
which updated two concept files but left the two Go doc comments and the API catalogue behind.
All four were genuinely stale, and the stale text was the kind that misleads specifically: it told
a reader that a broken connection would be reported healthy, which is the exact failure class this
PR exists to close.

Corrected in `VerifyAccountOrgReference` (`internal/platform/linkedin/accounts.go`) and on the
`OrgReferenceVerifier` interface (`internal/service/orchestrator.go`), both of which still listed a
malformed configured org id among the `nil` outcomes; in
[api-catalog.md](../../api-catalog.md), whose LinkedIn `/test` row named it as something `OK: true`
could be hiding; and in [internal-service.md](../code/internal-service.md).

`internal-service.md` carried a second, separate error the same pass caught: it listed a MISSING
configured org id as a `nil` outcome. That was never true at any point in this PR.
`LinkedInDispatcher.VerifyAccountOrg` (`internal/dispatch/linkedin.go`) rejects an empty account id
or org id with a real error before the client is called at all, so a missing org id has always
failed the test rather than passing it silently.

`nil` from this check now means exactly two things, and the four sources now say so identically: a
CONFIRMED match, or LinkedIn having no comparable reference on the account (empty, or
person-scoped). Those two remain indistinguishable to a caller by design, which is why the success
message says "no mismatch found" rather than "verified".

Documentation only — no behaviour change. Verified clean: `make check-fmt`,
`golangci-lint run ./...` (0 issues), `go build ./...`, `go test ./...`, and
`go run ./cmd/okfvalidate ./docs/knowledge`.
