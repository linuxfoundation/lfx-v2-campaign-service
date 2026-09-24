# 2026-09-25 A malformed configured org id fails the LinkedIn connection test

**Fix** — A fourth Copilot pass on PR #223 flagged, in code that had not changed since the
previous review, that `VerifyAccountOrgReference` (`internal/platform/linkedin/accounts.go`)
returned `nil` for a configured org id failing `orgIDRE`, so `TestLinkedinAds` reported `OK: true`
for it.

The finding was correct, and the reasoning it replaces is worth recording because it was sound
about the wrong question. The guard was added in an earlier round of this same PR to stop a
non-numeric `configuredOrgID` being compared against LinkedIn's always-numeric `reference` and
reported as a CONFIRMED disagreement — a value that was never a comparable org id cannot be the
DIFFERENT organization a mismatch requires. That is still true. But "is there a mismatch?" is not
the only question the connection test answers: `orgIDRE` is this client's configuration
invariant, and `resolveOrgID` (`targeting.go:196`) refuses the identical value because it cannot
build a valid `urn:li:organization:<id>`. A connection carrying one is therefore already
guaranteed to fail campaign creation — the precise "broken connection reported healthy" outcome
this verification exists to prevent, and the same class as the 403/credential/early-page cases
closed earlier in this PR.

`VerifyAccountOrgReference` now returns a confirmed (unwrapped, non-sentinel) error for a
non-numeric configured org id, refused BEFORE the walk: the verdict follows from the stored value
alone, so spending a round trip would only make a decided answer depend on that call succeeding.
`TestLinkedinAds`'s `default` arm reports it as an ordinary failed test (`OK: false`), not a 5xx,
which is the right shape — the thing under test is broken, not this service.

The error deliberately does not describe the fault as a mismatch. Naming a "different
organization" for a value that never parsed as an org id would send an operator hunting a tenant
mixup instead of fixing a malformed field.

The existing subtest was rewritten from `malformed configured org id is inconclusive, not a
confirmed mismatch` to `... fails the test, and is refused before enumeration`, and now also
asserts the error is NOT `ErrOrgVerificationInconclusive` (which maps to `OK: true`, the bug) and
that the fake server received zero requests. Its fixture is the full URN
`urn:li:organization:2414183` — the realistic mistyping, and one that CONTAINS the correct digits,
so a laxer check that merely looked for the numeric id inside the string would wrongly pass it.

Concept docs updated: [internal-platform-linkedin.md](../code/internal-platform-linkedin.md)
(the outcome table no longer lists a malformed org id as inconclusive, and records why the
earlier reasoning was incomplete) and [internal-service.md](../code/internal-service.md) (the
`nil`-folds-inconclusive-outcomes paragraph).

Verified clean: `make check-fmt`, `golangci-lint run ./...` (0 issues), `go build ./...`,
`go test ./...` all pass, and `go run ./cmd/okfvalidate ./docs/knowledge` is conformant.
