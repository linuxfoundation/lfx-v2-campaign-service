# 2026-08-19 — LFXV2-3281 token-decode redaction and the bootstrap refresh trio

**Fix** — three findings from the local reviewer trio and the human review. Each was verified
against the running code before being acted on, and one reviewer claim was falsified.

## The 2xx decode was the last arm of fetchToken still wrapping its cause

Every other arm redacts: transport via `redactHTTPDoError`, body-read via
`redactBodyReadError`, non-2xx by reporting status only — each carrying a comment explaining
that the token request's body holds `client_secret` and the refresh token, and that these
errors persist durably into a campaign's `Steps`. The decode at the bottom of `fetchToken`
still did `fmt.Errorf("decode linkedin token response: %w", err)`, on the one body that
carries `access_token` and the rotated `refresh_token`.

**What is actually reachable, tested rather than argued.** The reviewers disagreed about
severity, so the claim was settled by running `encoding/json` directly:

- `json.UnmarshalTypeError` renders the field's Go TYPE, not its value — a string value
  smuggles nothing.
- It DOES reproduce an out-of-range **number literal** verbatim and unbounded: a 200-digit
  literal renders in full.
- `SyntaxError` echoes exactly one offending character.
- A real token value cannot reach the text by any shape tried — as a number field, a wrong-typed
  string, an object key, or trailing garbage. Any non-numeric byte fails as a syntax error
  *before* the literal is rendered.

So the reviewer framing of "a live credential leak" is **false**: a credential is not
all-digits and is not expressible here. What is real is unbounded untrusted upstream text
reaching a persisted error, and a divergence from the discipline every sibling arm follows.
Redacted for that reason — the arm now reports `malformed JSON (%d bytes)`, matching the
decode in `metrics.go`, which already refuses to wrap for the same reason.

`TestTokenDecodeErrorNeverEchoesTheResponseBody` covers it. The existing
`TestRefreshErrorNeverLeaksCredentials` could not: it serves **400**, so it returns at the
non-2xx arm and never reaches the decode — the coverage gap that let this through is the same
shape as the previous round's (a leak test that exercised one sink and passed throughout).
The new test walks every `errors.Unwrap` layer, since a credential-bearing wrapper is exactly
what a check on the outermost layer alone would miss.

**Mutation:** restoring the `%w` fails the new test at TWO layers (`*fmt.wrapError` and
`*json.UnmarshalTypeError`), reproducing the literal in full. The guard bites.

## The bootstrap installer bypassed the all-or-none refresh rule

`validateLinkedInRefreshCredentials` (`internal/service/connection.go`) enforces that
`refresh_token`, `client_id` and `client_secret` are supplied together or not at all — but
only on the two public handlers. The third write path, the system-account installer, has its
own validator: `requiredCredentialKeys[ProviderLinkedInAds]` is `{"access_token"}` and nothing
checked the trio. Confirmed by grep: no `CanRefresh` or all-or-none logic existed anywhere
under `internal/bootstrap/`.

This is the highest-blast-radius row in the deployment — the fallback for every project that
has connected no account of its own. An operator paste dropping one field installed at exit 0,
silently failed `CanRefresh()`, and would resurface ~60 days later as exactly the expired-token
outage this ticket was opened for.

`conditionalCredentialGroups` + `validateConditionalGroups` now mirror the API rule, as a
general mechanism rather than a LinkedIn special case: a group whose members are each
individually optional but which is invalid when only some are present, which
`requiredCredentialKeys` structurally cannot express. A member present but empty or
whitespace-only counts as ABSENT, matching the required-key loop, so it cannot half-satisfy
the group.

Both directions are tested. Six partial shapes are rejected; `bearer only` and `full refresh
trio` must still install — without that second test the rejection table would be satisfied by
a guard that refuses every LinkedIn install.

**Mutation:** disabling the guard fails all six partial-trio cases and no others.

Worth recording from writing those tests: LinkedIn additionally requires a digits-only
`account_id` and an `org_id` config, both refused *before* the credential group is reached.
The first draft of the positive test failed for that reason, not because the guard was wrong.

## The catalog documented the new 409 but not the new 400

`docs/api-catalog.md` gained the `credentials_expired` 409 reason on the toggle and metrics
rows, so the catalog was in scope for this change — but the all-or-none **400**, a new runtime
rejection with no OpenAPI counterpart (Goa's `Required` cannot express a conditional group),
was undocumented. Added as a note alongside the connection rows.

## Falsified — no change made

A suppressed reviewer comment claimed `ListAccounts` "discards the `resolved` value and
therefore uses a generic label and calls `linkedinExpiry` without `res.systemScoped`". Read at
the target, `internal/dispatch/linkedin.go` already passes `linkedinConnectionLabel(res)` to
the client and already returns `res.systemScoped(linkedinExpiry(lerr))`. The comment describes
an earlier revision. No change made; recorded so it is not re-raised.
