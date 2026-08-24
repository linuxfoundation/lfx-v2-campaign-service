# 2026-08-24 — LFXV2-3033: a filter that matches no test reports PASS

**Verification** — Re-synced this branch onto `origin/main` (merge of `3c01b1b3`)
and re-ran the per-provider mutation diagonal that guards the compare-and-clear
token invalidation across Reddit, Microsoft and Google Ads.

The first run of that diagonal reported every mutation as SURVIVED: all three
providers green while each provider's compare guard was deleted in turn. That
result was an artifact of the harness, not of the code. The runs were filtered
with `go test -run TokenInvalidation`, and **no test in any of the three
packages has that substring in its name** — the real names are
`TestABA_LateUnauthorizedDoesNotEvictANewerToken`,
`TestInvalidate_DoesNotStrandANewerFlight` and the `401Invalidates` family. A
`-run` pattern matching zero tests exits 0 and prints `ok`, so a filter typo is
indistinguishable from a passing suite at the exit-code level.

The lesson generalises past this branch: **a mutation that "survives" is only
evidence if the harness is first shown to be capable of failing.** Before
reading a survivor as a gap in the tests, self-test the selector — count the
tests the filter actually matched (`-v | grep -c '^=== RUN'`) and assert it is
non-zero. Here the corrected selector matched 6 tests per package, and re-running
the diagonal against the **whole package** (no `-run` at all, which removes the
failure mode at its source) produced the intended result: each provider's
mutation fails that provider's package and only that one, a clean diagonal with
no off-diagonal failures and no survivors.

Reverts were verified by reading the restored lines back, not by trusting the
copy's exit code: all three guards are present at `reddit/client.go:643`,
`microsoft/client.go:582` and `googleads/client.go:444`, and the working tree is
clean.
