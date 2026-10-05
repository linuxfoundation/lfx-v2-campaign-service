# 2026-10-02 — LFXV2-2023 account-scope tests: live vendor calls, and what the arms actually bind

**Fix** — Follow-up on the account-scope guard recorded in
`2026-10-02-LFXV2-2023-account-scope-guard.md`, from the local review cycle. The guard itself
is unchanged; this is about the tests that were supposed to pin it.

**The tests were calling the live vendor APIs.** Both account-scope tables built their
dispatcher with no `WithBaseURL`, so `ListAccountCampaignMetrics` reached the real
`api.linkedin.com` and `graph.facebook.com`. The permitted arms "passed" only because the
vendor answered 401 to a fixture credential — with egress blocked
(`HTTPS_PROXY=http://127.0.0.1:9`) both LinkedIn permitted arms FAIL. That is red in CI
without egress, flaky with it, and it silently re-scopes what the test proves onto a vendor's
current auth behaviour. Both tables now drive a local `httptest` stub through
`linkedin.WithBaseURL` / `meta.WithBaseURL`, the pattern each file already used (26 and 36
other uses respectively). All 11 arms pass with the network blocked.

The handler captures the first request path under a `sync.Mutex` and every read takes the
same lock — the same hazard and the same remedy as
`2026-08-05-lfxv2-2023-ga3b-test-race-and-api-catalog.md`, which fixed a captured-request-path
race in an httptest handler on this same ticket. `t.Fatalf` is never called from the handler
goroutine.

**A stronger assertion was available and the comment had denied it.** The previous comment
claimed the permitted arms' weakness was "inherent to a permitted path, not a gap to fix".
That was wrong: the offline run shows the client-built path is in the error with no network at
all, so a permitted arm can assert the upstream PATH reached. It now does, which pins that the
account forwarded upstream is the one the connection STORES — a guard that validated the
stored id but forwarded the caller's would fail. Refusal arms additionally assert the stub was
never called, pinning that refusal precedes any request; no earlier arm checked that.

What no permitted-path assertion can detect is a guard that is MISSING altogether, since "the
read proceeds to the stored account" is equally true with none. Only the refusal arms are
sensitive to that. Mutation confirms both directions: removing the guard call fails 4 arms;
disabling only the mismatch comparison fails 1. The comments now claim that residual limit
rather than denying a fixable gap.

**Shared-account regression test.** `TestMeta_ListAccountCampaignMetrics_SharedAccountAcrossProjects`
pins the configuration the guard must NOT break, which is the live one: Meta is a single ad
account shared across foundations, so two different projects legitimately store the SAME
`account_id` and each must be permitted to read it. Refusing a request because another project
also uses that account would answer 400 for every shared-platform read — a worse defect than
the cross-project read the guard prevents, and the direction a later "tightening" would
plausibly take. The guard is safe because it compares against the project's own stored
`account_id`, which on a shared platform IS the shared account.

**Note on the earlier fragment.** `2026-10-02-LFXV2-2023-account-scope-guard.md` was amended in
the same commit to widen one claim it already made: `docs/architecture.md` carries the stale
per-foundation claim in BOTH its "Account Tenancy" and "Current Platform Accounts" tables, and
the original text named only the first. Widening a claim inside one's own entry is not a new
entry; the substantive changes above are, which is why they are filed here instead.
