# 2026-09-26 — LFXV2-2665: finishing the `ok` rationale sweep, and bounding the token-handoff waits

**Fix** — Round 10 of the connection-probe work. Two findings, and the first is the round-9
correction not having reached everywhere it applies.

## The credential-only justification survived in five more places

Round 9 replaced the justification for `OK: false` on an inconclusive check — from "the credential
did not authenticate" to the CONJUNCTION the field is actually declared as, the credential
authenticated AND the configured account passed that provider's own check. It changed the sites the
sweep found. It did not change every site, because the sentence is written slightly differently in
each one and a grep for the operator-facing phrasing does not catch a comment that paraphrases it.

Three were named by review: `docs/api-catalog.md`, `docs/knowledge/code/internal-service.md` and
`internal/service/connection.go`'s inconclusive arm. A sweep for the CLAIM rather than the wording
— `declared as whether the credential`, and `credential authenticated against the provider` in
either case — found two more that were not: `internal/domain/errors.go`'s
`ErrOrgVerificationInconclusive` doc and `internal/service/connection_probe_test.go`'s
inconclusive subtest.

The `errors.go` one is the sharpest instance in the repo, because it contradicted itself two lines
apart: "the credential baseline already passed" immediately above "`ok` is declared as whether the
credential authenticated against the provider and an incomplete walk did not establish that." Both
sentences cannot be true. That is the whole round-9 finding, written down in one paragraph.

The lesson is about how the sweep was run, not about the contract: a claim duplicated across a
design file, a generated catalog, two bundle concepts, a domain sentinel, a service arm and a test
comment is found by searching for what it ASSERTS, not for the sentence that asserts it. The
round-8 log entry is left untouched — a dated entry records what was believed on its date, and the
round-9 entry is where the correction lives.

## Three token-refresh tests could hang the package instead of failing

`token_refresh_scope_test.go` in `googleads`, `microsoft` and `reddit` cancels the caller's context
only once the refresh is genuinely in flight — cancelling earlier is refused by the already-done
guard and proves nothing — and waited for that moment on a bare `<-reached`.

Both later receives in each of those tests are already bounded by
`refreshScopeObservationWindow + 2s`; the first one was not. A regression that stops the request
ever leaving the client therefore hangs the package rather than failing the test, and under
`go test -race`, which `make test` runs, that surfaces as some unrelated test timing out on a
loaded runner — a failure pointing at the wrong code. Each bare receive is now the same bounded
`select` as its siblings, failing with "the token endpoint was never reached."

This was confirmed rather than assumed: with `close(reached)` removed from the `googleads` handler,
the bounded form fails both subtests in 2.5s naming the token endpoint, where the bare form would
have blocked until the suite's own timeout. Matched
`httptest-handler-state-needs-synchronized-handoff` in
`docs/reviews/knowledge-base/test-hygiene.md`, whose detect condition names the unbounded receive
explicitly alongside the fixed `time.Sleep`.
