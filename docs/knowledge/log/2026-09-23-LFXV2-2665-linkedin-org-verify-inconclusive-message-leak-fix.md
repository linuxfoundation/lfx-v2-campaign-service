# 2026-09-23 LinkedIn org-verification: inconclusive message no longer echoes the transport error

**Fix** — The local-review learnings pass on PR #223's round-2 fix commit
(`docs/reviews/knowledge-base/credentials-and-untrusted-text.md`'s
`platform-error-must-not-carry-untrusted-or-credential-text` pattern) flagged that
`TestLinkedinAds` (`internal/service/connection.go`) built its `OK: true` advisory message for
the `linkedin.ErrOrgVerificationInconclusive` case by concatenating `verr.Error()` directly into
the HTTP response. That error can originate from `ListAdAccounts`' `*transportError`
(`internal/platform/linkedin/client.go`), whose `Error()` renders the underlying `*url.Error`
verbatim via `%v` — including the full LinkedIn request URL and any query parameters (e.g. a
pagination cursor). Not a bearer token (that rides in the `Authorization` header, never the URL),
but still not this endpoint's to disclose, and inconsistent with the `ErrCredentialDecryptionFailed`
and `ErrServiceDefect` arms four lines below, which already withhold `verr.Error()` from the
response for the same reason.

Fixed by logging the raw error server-side only, via `slog.WarnContext` with a structured
`"error"` field — the same pattern the file's `classifyDiscoveryError` default arm already uses
for an equivalent upstream failure — and returning a fixed advisory message with no error text
to the caller.

Added a regression to `internal/service/connection_test.go`
(`TestTestLinkedinAds_UpstreamVerification/inconclusive enumeration failure never echoes the
transport error's URL into the response`) that manufactures an inconclusive error wrapping a
transport error with a marker URL and query string, and asserts neither appears in the response
message.

Verified clean: `gofmt -l .`, `go vet ./...`, `go build ./...`, and
`go test ./internal/service/... ./internal/platform/linkedin/... ./internal/dispatch/...` all
pass, including the new regression.
