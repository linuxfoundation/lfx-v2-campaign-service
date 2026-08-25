# 2026-08-24 — LFXV2-2643 the untested half of a redactor

**Fix** — syncing this branch onto `origin/main` was a clean auto-merge with no conflicts,
so the merge itself needed no resolution. Verifying it did: symbol arithmetic over `^func `
across all 54 Go files the merge touched found nothing present that neither side
contributed and nothing lost that both shared, and `go build`, `go vet` and `gofmt -s`
were all clean before any repair.

The finding is in what the merge was asked to PROTECT. `SafeDSNErr` has two arms — a
`*url.Error` cause is replaced wholesale by a fixed sentinel, because `net/url` causes
quote the fragment they choked on and an unwrap-and-scrub still leaks the password; every
other cause keeps its own text through `redact.URLUserinfo`. Both halves are load-bearing,
and only the first was tested.

`TestSafeDSNErrKeepsCredentialsOutOfOutput` and
`TestSafeDSNErrRedactsAWrappedMigratorError` both construct `*url.Error` inputs. Neither
ever reaches the fallback `return`, so that line was unconstrained: replacing it with

    return "withheld"

compiles, passes `go vet`, and leaves BOTH existing tests green. That mutation is not
hypothetical vandalism — it is the shape a well-meaning "redact harder" change takes, and
it produces a helper that leaks nothing and diagnoses nothing. Connection refused,
authentication failed and database-does-not-exist are the errors an operator actually
meets; none is a `*url.Error`, and their text is the entire diagnosis. It does not carry
the credential, because the DSN reaches a cause only when `net/url` quoted it, which is the
arm the sentinel already covers.

`TestSafeDSNErrKeepsDriverTextForNonURLErrors` closes it, asserting two-sidedly over four
causes: the driver's own words must SURVIVE, the `*url.Error` sentinel must NOT be used for
a cause that is not one, and the password must stay out. A leak and a silent constant both
fail it.

**Verification** — the new test passes against the current code and fails on all four arms
against the mutation that previously survived, so it constrains the line rather than
agreeing with it. The redacting arm is unchanged and still bound: reverting the sentinel to
`redact.URLUserinfo(ue.Error())` fails seven cases across the three tests. Both mutations
compiled and were reverted, with `git diff` confirming each restoration.

The merge is otherwise a base sync with no behavioural change: main's two new commits are
the `000027` provenance migration and the LinkedIn token refresh, and neither touches this
package.
