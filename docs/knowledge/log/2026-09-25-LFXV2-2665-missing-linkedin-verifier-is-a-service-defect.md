# 2026-09-25 A missing LinkedIn org verifier is a service defect, not a silent pass

**Fix** — A sixth Copilot pass on PR #223 found that `Orchestrator.VerifyAccountOrg`
(`internal/service/orchestrator.go`) swallowed its own failure mode. It returns `nil` when the
platform has no registered dispatcher, or when the registered one does not implement
`OrgReferenceVerifier`. That silence is correct for the 5 platforms with no upstream org
reference to check — but it applied to LinkedIn too, the one platform the check exists for.

Nothing upstream caught it. `testConn` (`internal/service/connection_handler.go`) reads only the
stored row and never touches a dispatcher, and `resolveBackendWithOrch` checks only that the
orchestrator pointer is non-nil. So a build with the LinkedIn dispatcher missing from the registry
would pass the credential baseline, skip the cross-check without a word, and answer `OK: true`
with "no linkedin account/organization mismatch found" — the "broken connection reported healthy"
outcome this whole verification exists to prevent, and worse than the earlier instances because
nothing in the response or the logs distinguished it from a real pass.

The permissiveness is now scoped by `orgVerificationRequired`, a map naming the platforms whose
dispatcher MUST implement the interface. Membership is a claim about the PLATFORM, not about this
service's wiring: LinkedIn's ad-account resource EXPOSES a `reference` field, so there is a check
to run and a build that cannot run it is mis-wired, whereas Google or Reddit being absent is the
designed outcome. Membership says the check must RUN, not that it must reach a verdict —
`reference` is optional per account, and one that omits it is precisely the inconclusive `nil` the
outcome model documents.
For a required platform both paths now return `domain.ErrServiceDefect`, which `TestLinkedinAds`
already maps to a typed 500 with a log line saying the stored connection is NOT at fault — the
right destination, because the operator needs to fix a wiring bug, not audit connection fields.

`TestOrchestrator_VerifyAccountOrg_NoOpForUnregisteredPlatform` pinned the old behaviour using
LinkedIn as its fixture, which was the wrong platform for the claim it was making — the no-op is
only defensible where nothing is being skipped. It now uses Google, and a new
`TestOrchestrator_VerifyAccountOrg_MissingRequiredDispatcherIsAServiceDefect` covers both LinkedIn
paths. `..._NoOpForUnsupportedPlatform` was already correct and is unchanged.

The same pass also corrected a stale comment in `TestLinkedinAds` (`internal/service/connection.go`)
that still described `nil` as covering "several" inconclusive outcomes. After the 2026-09-25
contract sweep there is exactly one: LinkedIn having no comparable reference on the account.

Concept doc updated: [internal-service.md](../code/internal-service.md).

Verified clean: `make check-fmt`, `golangci-lint run ./...` (0 issues), `go build ./...`,
`go test ./...`, and `go run ./cmd/okfvalidate ./docs/knowledge`.
