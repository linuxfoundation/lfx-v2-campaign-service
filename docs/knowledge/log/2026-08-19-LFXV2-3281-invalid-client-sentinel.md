# 2026-08-19 — LFXV2-3281 invalid_client needs a sentinel, not just a split

**Fix** — the `invalid_client` arm landed at head `fff642bf` dropped out of the sentinel
chain, leaving the fault LESS classifiable than before the split that introduced it. Reported
by Cursor Bugbot (High) against `token.go:303-314`.

Splitting `invalid_client` (operator misconfiguration) from `invalid_grant` (expired or
revoked) is RIGHT and is kept — a token endpoint answers 400/401 for both, and the two have
opposite remedies, so classifying on status alone told an operator whose `client_id` held a
typo to re-authorize a member, which could never help. The defect was the shape of the new
arm, not the decision behind it.

It returned a bare `fmt.Errorf`. That unwraps to nothing, and **every arm that acts on this
classification matches structurally** — so the error carried neither a reason sentinel nor
`domain.ErrConnectionNotUsable`, and fell through to the generic retryable path: a 503
telling a caller to retry a condition that cannot clear until someone edits the connection.
That is the same opaque retry surface the split existed to retire. On the LF SYSTEM
connection it is one typo disabling LinkedIn for every project falling back to it, reported
as a transient outage.

The remedy is a sentinel of its own at both layers, never a re-use of the expired one:

- `linkedin.ErrApplicationCredentialsInvalid` with a typed `applicationCredentialsError`
  carrying only the operator-set connection label. It records the token-exchange status for
  classification and does not render it. It carries no `Method`, unlike
  `credentialsExpiredError`: this arm is reachable only from the token exchange, which
  happens BEFORE the request it would authorize, so nothing was sent and no outcome can be
  ambiguous — `createOutcomeAmbiguous` reports false for it with no special case.
- `domain.ErrApplicationCredentialsInvalid`, wrapped ALONGSIDE `ErrConnectionNotUsable` like
  every other reason token here, so that sentinel keeps deciding the status while this one
  carries the machine-readable reason. It is deliberately NOT `ErrCredentialsExpired`, which
  resolves to "the member must re-authorize" — actionable, and provably unable to repair an
  application credential.

**The class was fixed across all four re-tagging sites the finding names, not just the
first.** `linkedinExpiry` now re-tags both defects (checking the application-credential arm
FIRST, so if an error ever carried both, the operator-actionable reading wins over a remedy
that cannot work). But the helper was never the whole story: three of the four sites —
Dispatch, ToggleStatus, ReadMetrics — guard with an `if` BEFORE calling it, and a guard that
still asked only about expiry would strand the fault at exactly that site regardless of what
the helper does. They now guard on `linkedinConnectionDefect`, which recognises both.
ListAccounts already applied `linkedinExpiry` unconditionally and needed no guard change.
`unusableConnectionReason` gained the matching case, ordered before the expired arm, so the
log token is `application_credentials_invalid` rather than `unclassified`.

No upstream error text is echoed on any arm; only the classification travels, and the test
asserts that a secret-shaped `error_description` does not reach the message.

**Verification** — two mutations, each compiling, each reverted:

- Restoring the bare `fmt.Errorf` fails all four subtests of
  `TestLinkedIn_InvalidClientIsTaggedOnEveryPath` plus the client-level
  `TestInvalidClientIsNotReportedAsAnExpiredCredential`. The failure output is the reported
  defect itself: the message arrives at every caller with no sentinel behind it.
- Reverting the three guards to `errors.Is(..., ErrCredentialsExpired)` fails Dispatch,
  ToggleStatus and ReadMetrics while ListAccounts still passes — confirming the guards are
  load-bearing at exactly the three sites that have one, and that testing the helper alone
  would have proved nothing about them.

The client-level test previously asserted the classification with
`strings.Contains(err.Error(), "invalid_client")`. That is the weak form — it passes against
an error that unwraps to nothing, which is how the defect survived its own test. It now
asserts `errors.Is(err, ErrApplicationCredentialsInvalid)`.
