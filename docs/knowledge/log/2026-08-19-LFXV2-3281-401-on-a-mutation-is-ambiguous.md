# 2026-08-19 — LFXV2-3281 a 401 on a mutating request is ambiguous, not definite

**Fix** — three review findings that turned out to be one defect: the 401 handling collapsed
two independent facts into one, and the surviving fact was the less actionable of the pair.

This does NOT revisit the fail-closed decision recorded in
`2026-08-19-LFXV2-3281-401-fails-closed-by-design.md`. The rejected request is still never
replayed inside the failing operation, and `TestRefreshCapable401FailsClosedWithoutReplay`
still passes untouched. What changes is only how the resulting error is CLASSIFIED.

## A 401 answering a POST says two things

"Reconnect this connection" is the actionable half. The other half was being discarded:
LinkedIn "reserves the right to revoke Refresh Tokens or Access Tokens at any time", so a
revocation can take effect between LinkedIn committing a create POST and writing its
response. A 401 on a mutation therefore says nothing about whether the write landed — the
same position a mutating 5xx leaves the caller in, which `createOutcomeAmbiguous` has always
treated as ambiguous.

`doRequest`'s 401 arm built a `credentialsExpiredError` from the connection label and a
reason string, dropping the in-scope `method` and `resp.StatusCode`. `createOutcomeAmbiguous`
matched only `transportError` and `apiError`, so it answered false for every 401.

The consequence was concrete rather than theoretical. `CreateCampaign` returned a clean
`(nil, err)`, the dispatcher took its `result == nil` arm, and the dispatch claim was
RELEASED — for a campaign group LinkedIn may already have committed. A retry then creates a
second billable group, and nothing was told to look for the first.

`credentialsExpiredError` now carries `Method` and `StatusCode`, and
`createOutcomeAmbiguous` gained an arm reading them under the SAME method gate the other two
arms use. The fields are never rendered by `Error()`: they exist for classification, and the
operator-facing message stays the single actionable "reconnect" sentence.

The method gate is what keeps this from becoming "every 401 is ambiguous":

- POST/PUT/PATCH/DELETE 401 → ambiguous (the mutation may have committed)
- GET 401 → definite (a search POSTed nothing; a phantom group would send an operator
  hunting a resource that does not exist)
- PRE-SEND expiry → definite, and it needs no special case. The three pre-send arms (a
  known-past access-token expiry, an expired refresh token, a rejected token exchange) leave
  both fields ZERO, and a zero method is not a mutating method.

The claim-retention rule is untouched: it still keys on `result == nil` ALONE, never on
whether the campaign id is populated. The 401 now produces a non-nil name-only partial with
an empty `CampaignID`, which is exactly the shape the ambiguous-create path already returned.

## The expiry branch was preempting the unconfirmed branch

The same collapse appeared in reverse in the dispatcher's `ToggleStatus`. The cascade is
multi-step, so a credential can die BETWEEN steps: on PAUSE the campaign gate is flipped
first and the creatives second, so a mid-cascade 401 arrives wrapped in a
`partialCascadeError` whose `Unwrap` exposes the inner expiry. The `errors.Is(uerr,
ErrCredentialsExpired)` check sat above `IsOutcomeUnconfirmed`, so it matched first — after
the pause had already taken effect — and the caller was told only to reconnect.

The masking went further than losing a message. `linkedinExpiry` tags the error with
`domain.ErrConnectionNotUsable`, and the service's toggle switch matches THAT sentinel above
its own unconfirmed arm, so a half-applied cascade answered a non-retryable 409 "repair your
connection". The unconfirmed arm's 503, its reconcile-signal log, and its
`ReleaseCampaignLockAfterCooldown` hold were all skipped.

Both facts are true, so the question is which one a caller can act on — and only one of them
is perishable. "Verify the platform state before retrying" describes a partial effect that
persists whether or not the credential is ever repaired; "reconnect" is a precondition the
very next call rediscovers. So the unconfirmed check moved above the expiry check, and
nothing is lost by it: `unconfirmedToggleError` wraps the cause, so
`errors.Is(err, ErrCredentialsExpired)` still answers true and the service layer can report
both. A pre-send expiry is not unconfirmed, so it falls through and keeps answering
"reconnect", unchanged.

`IsNotServable` stays above both — an activate refused before any mutation ran is neither
ambiguous nor a credential problem.

## Mutations

Each was written to COMPILE, since a build break proves nothing about a test.

1. `createOutcomeAmbiguous`'s new arm → `return false` (the pre-fix classifier): fails the
   classifier table on all four mutating cases, the POST half of the wrap-site test, and both
   end-to-end claim-retention tests (`result = nil`).
2. Drop `Method`/`StatusCode` at the `doRequest` 401 wrap site: fails with `Method = "", want
   "POST"` and `StatusCode = 0, want 401`, and both retention tests go nil again — proving
   the classifier and the plumbing are separately load-bearing.
3. Restore the expiry check above the unconfirmed check: the toggle test fails with the
   defect rendered in full — the surfaced error carries `ErrConnectionNotUsable` and the word
   "unconfirmed" appears nowhere.
4. `return true` in place of the method gate: fails the GET-401 and pre-send rows, the GET
   half of the wrap-site test, `TestCreateCampaign_GETSearch401StaysDefinite` (which now
   reports a phantom group), and the pre-send toggle test. The negative cases are load-bearing
   at every layer, not decoration.

No existing test asserted the old behaviour, and this was checked rather than assumed. The
four 401 sites under test are all pre-send or GET paths —
`TestLinkedIn_ExpiredCredentialsAreTaggedOnEveryPath` drives all four dispatcher entry points
with a refresh token already past its deadline, so it never sends a request — which is why
the fix leaves them green rather than needing them rewritten. That gap is also exactly why
the defect survived: the response-arm 401 on a MUTATING request had no test at all.
