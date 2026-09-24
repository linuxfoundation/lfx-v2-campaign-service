# 2026-09-25 The 403-only narrowing left five stale "every 4xx is a verdict" claims

**Fix** — Narrowing the confirmed-verdict allowlist to `403` changed what four other places in
the repo were asserting, and only some of them were updated in the same pass. A later review
pass on PR #223 found the remainder: `internal/domain/errors.go` said "the 4xx refusals carry
the confirmed marker" on `ErrOrgVerificationInconclusive` and listed "a non-429 4xx refusal
LinkedIn reached on the merits" as a verdict on `ErrOrgVerificationFailed`; the matching comment
in `TestLinkedinAds` said the same; and the summary paragraph in
[`internal-service.md`](../code/internal-service.md) still described a two-way outcome split.

These are exported sentinel docs, which is what makes them worth a fix rather than a follow-up.
They are the contract a future caller reads before deciding which sentinel to attach, and both
now say the wrong thing in the direction that reopens the original defect: a reader who takes
"every non-429 4xx is a verdict" at face value and tags a `400` with `ErrOrgVerificationFailed`
lands it back in the echo allowlist, sending an operator to audit a correct configuration. All
three Go sites now state the split and say WHY a `400`/`404` is not a verdict — the walk embeds
neither the stored account id nor the configured org id — so the reasoning travels with the rule
instead of living only in a log fragment. The concept doc's summary was replaced with a
four-row outcome table (confirmed verdict, credential failure, inconclusive, service defect)
plus the `503` cases, because an operator reading the old paragraph would take the wrong retry
semantics from it.

The same pass caught a wrong operator-facing MESSAGE. `TestLinkedinAds` returns early when
`testConn` reports `!OK`, and `testConn` is shared across providers: its `OK` is exactly
`HasCredentials()`, and its message says "connection found; upstream verification not yet
implemented". For LinkedIn both halves are false — verification is implemented and runs three
lines later, and the real reason for `OK: false` is an absent credential. An operator reading it
goes looking for an unimplemented feature instead of authorizing the connection. The LinkedIn arm
now substitutes a message naming the absent credential and the remedy, leaving the shared helper
untouched for providers that genuinely have no upstream check; `testConn`'s own doc comment now
states that a verifying caller is expected to replace the message. The existing
`TestTestLinkedinAds_NoCredentialsSkipsUpstreamVerification` asserted only `OK == false`, so it
passed with the misleading text — it now asserts the message names the reason and no longer
claims verification is unimplemented.
