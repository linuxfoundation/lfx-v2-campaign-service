# 2026-08-10 — Four stored-connection defects that answered 503

**Update** — `internal/dispatch/{reddit,twitter,microsoft}.go` (LFXV2-3069, part 1). The shared
pre-flight in each of those three adapters now tags every stored-connection defect it detects
with `domain.ErrConnectionNotUsable` plus a reason sentinel, the way Google Ads already did.
Meta and LinkedIn are part 2 and still bare.

## The bug was a missing tag, and it was invisible from inside the adapter

Each adapter checks four things before contacting a platform: the row is `active`, the decrypted
blob is valid JSON, the decoded credentials have every required field, and an ad account is
selected. All four were detected correctly. All four returned a bare `fmt.Errorf`.

The service layer has exactly one arm for an error it does not recognize, and it answers **503**.
So a connection whose credentials were saved with a missing field told the caller that a platform
had not responded — about a platform no request was ever sent to — and prescribed retrying, which
is the one remedy that cannot work: only a human editing the connection clears any of these. The
correct arm, 409, already existed at `internal/service/brief.go`; nothing reached it because the
classification is only recoverable in the layer that knows the failure was pre-send.

Reddit and X/Twitter each route dispatch, `ToggleStatus` and `ReadMetrics` through one helper, so
one tag fixed three endpoints per adapter. Microsoft has no metrics path (async Reporting API),
so it fixed two.

## Both sentinels, not one

`ErrConnectionNotUsable` selects the status. It is not enough on its own: the handler logs
`reason=` from `unusableConnectionReason`, a fixed vocabulary, and an error carrying only the
status marker logs `reason=unclassified` — the most diagnosable states in the set becoming the
least diagnosed. So each defect wraps a second sentinel too (`ErrConnectionInactive`,
`ErrCredentialsUndecodable`, `ErrCredentialsIncomplete`, `ErrAccountNotSelected`).

## The unmarshal cause is dropped, not wrapped

The previous code said `fmt.Errorf("decode reddit credentials: %w", err)`. That `err` comes from
decoding the **decrypted** credential blob, and `encoding/json` quotes its input: a
`*json.SyntaxError` names the offending character, a `*json.UnmarshalTypeError` names the field
it was reading. Left in the chain it is reachable by anything that renders or `errors.As`-walks
the error — including the 503 arm's own logging — for precisely the connection whose credentials
are malformed.

Nothing actionable is lost by dropping it. The remedy for an undecodable blob is "re-save the
credential", never "fix byte 41". The tests assert the absence with `errors.As` against both
concrete JSON error types rather than a substring match on `Error()`, because a cause still in
the chain is reachable even when the top-level string looks clean.

## The account-id branch was fixed too, though the ticket names only three

Each of the three helpers also had a bare "no account id" branch carrying the identical defect —
same wrong 503, same human-only remedy. It is tagged with `ErrAccountNotSelected`, matching the
Google Ads template. Two lines in a function already being rewritten, and it does not overlap
LFXV2-3061, which concerns Meta's Goa `Required("account_id")` and `accountDiscoveryProviders`
in different files. Leaving it would have shipped a helper that classifies three of its four
exits.

## Placement: after the resolve, and in the helper

Two ordering constraints, both easy to get wrong:

- `defer func() { err = res.systemScoped(err) }()` must come AFTER the `d.creds.resolve()` error
  check. `res` is nil before that point.
- The tagging belongs in the shared resolve/validate helper, not at each call site. Tagging
  per-path is how Google Ads ended up classified on discovery while its own dispatch and metrics
  callers were still bare, until the helper absorbed it.

## Verification

116 subtests: five defect fixtures × two credential scopes × every exported entry point of each
adapter (Dispatch, `ToggleStatus`, and `ReadMetrics` where wired). Each asserts `errors.Is` on both
sentinels and `errors.As` against neither JSON error type, and the `httptest` server fails the test
if the adapter reaches the network at all: rejecting locally is the property, not merely returning
the right error.

Three dimensions of the suite exist because a narrower one would have been vacuous:

- **Every entry point, not just the toggle.** "The tagging lives in the shared helper" is the whole
  design claim; driving one path leaves it unenforced. Inline the helper into a single caller now
  and the others go red.
- **Both credential scopes.** A project-owned fixture never sets `fromSystem`, so the deferred
  `systemScoped` is a no-op for it: all the project-scope cases stay green with the defer deleted.
  The LF-fallback scope is what pins it, and the project scope asserts the marker's ABSENCE, since
  a project told to repair the LF row is chasing something it cannot see. Deleting one defer fails
  22 subtests.
- **A wrong-TYPED credential field as well as `{`.** Malformed-brace input produces only a
  `*json.SyntaxError`, so a table with just that fixture never exercises the
  `*json.UnmarshalTypeError` assertion — and the type error is the one that names the credential
  FIELD.

Revert-verified against `origin/main`: every case fails there, including both leak classes.
