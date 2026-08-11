# 2026-08-10 — HubSpot metrics: doc echo + a decode-error leak path matching an existing guard

**Fix** — Two suppressed Copilot findings on PR #113, round 4.

## internal-service.md repeated the same pre-contact over-claim as api-catalog.md

Round 3 fixed `docs/api-catalog.md`'s 409 taxonomy but missed its twin in
`docs/knowledge/code/internal-service.md:164`, which made the identical claim —
`ErrCampaignAccountMismatch` refused "BEFORE contacting the platform" — now false for HubSpot's
path through `AuthenticatedPortalID`. Reworded to "before the tenant-scoped metrics request
itself" and added the same explicit note that `AuthenticatedPortalID` calls
`GET /account-info/v3/details` first.

## AuthenticatedPortalID's decode error could leak response fragments into logs

`account.go`'s `json.Unmarshal` failure wrapped the raw `encoding/json` error with `%w`.
`json.SyntaxError` and `json.UnmarshalTypeError` can reproduce fragments of the input in their
own messages, and both callers put this error somewhere that gets logged: `hubspot.go:176`
returns it wrapped further into `GetCampaignMetrics`'s default 503 arm, which logs via
`safeErrSummary(err)` (truncates, does not redact); `hubspot.go:283` logs `perr` directly in a
`slog.WarnContext` call. `internal/platform/hubspot/statistics.go:262-272` already solves this
exact problem for its own decode error — drop the cause, keep a byte count. Applied the same
fix here: `AuthenticatedPortalID` now returns `"read hubspot account details: response is not
valid JSON (%d bytes)"` with no wrapped cause.

## Tests

Doc-only change plus a message-text change with no behavior change. Grepped
`internal/platform/hubspot/*_test.go` and `internal/dispatch/*_test.go` for
`AuthenticatedPortalID` and the old error string — neither is asserted on by any existing test.
Ran the full suite plus `go run ./cmd/okfvalidate ./docs/knowledge` to confirm nothing
regressed.
