# 2026-09-29 — LFXV2-2665: four PR #231 review fixes to X authoring and snapshot redaction

**Fix** — Copilot's review of PR #231 returned four findings, all taken. Two are
redaction gaps the local review rounds did not reach; two are the same disproven
retry-safety claim in two places. Each now has a test that fails without its fix —
the first three flips landed green, which is itself what exposed that this retry
path had no coverage at all.

**1. `redactURLForError` kept the path.** It was written when its only caller was
an operator-typed registration URL and a query parameter was the only place a
secret could plausibly sit. That stopped being true when the same helper began
screening ARBITRARY caller copy on the `tweetText` path: a magic-link or reset
credential lives in a path segment — `https://example.com/reset/<secret>` — at
least as often as in a query. The knowledge-base rule
`caller-url-must-be-redacted-before-errors-steps-and-snapshots` is the test, and
the path fails it: reproduce a component only when it is BOTH structurally
incapable of holding a secret AND load-bearing for the diagnosis. A path is
capable; a host is not, and the host is what tells the operator which link to fix.
Scheme+host is the line. This was made package-wide rather than added as a second,
stronger redactor for the new branches only: dropping more can cost specificity but
can never leak, and a redactor whose strength depends on which caller reached it is
one nobody can reason about. `internal/platform/googleads/ad_copy.go` has a mirror
of this helper that still keeps the path — worth the same change, not made here
because that client's URLs do not flow into published text.

**2. `snapshotURLRunRe` cut IPv6 literal hosts in half.** `]` is one of the run
terminators, because it closes a markdown link, so
`https://[2001:db8::1]/reg?ticket=…` matched only as far as `https://[2001:db8::1`.
`sanitizeSnapshotURL` then saw no `?`, returned that fragment unchanged, and the
path AND query — the credential included — survived into the unencrypted
`config_snapshot` as ordinary prose. The pattern now tries a bracketed authority
first and falls back to the general run, so the terminator still ends a bracketed
ordinary URL (`[https://host/x?t=…]`) while an IPv6 authority is matched whole. All
three shapes are pinned in `TestSanitizeSnapshotText`.

**3 and 4. The campaign and line-item creates were marked retry-safe on reasoning
that does not hold.** Both passed `idempotent: true` to `createRequest`, justified
in the contract doc as "found-or-created by name, so re-issuing one converges".
Reading the call graph disproves it: the by-name lookup runs in `CreateCampaign`,
ABOVE `doRequestAbs`'s retry loop, and a retry from inside that loop re-POSTs
without repeating it. X does not dedupe campaign or line-item names itself, so if
it committed the write and then reported a 429 — which it does — the retry creates
a duplicate PAID resource, the precise outcome the by-name lookup exists to
prevent. Caller-side convergence is not retry safety; only SERVER-side convergence
is, which is why `promoted_tweets` stays `true` (X refuses the repeat with
`DUPLICATE_PROMOTABLE_ENTITY`) and tweet authoring stays `false`. Three of the four
creates now pass `false`.

No caller-side handling was needed for the flip: a 429 remains an `*apiError`, and
`createOutcomeAmbiguous` already classifies a mutating one as ambiguous, so both
paths already returned their reconcilable partial plus an UNCONFIRMED error. What
`false` removes is this layer performing an unverified retry on the operator's
behalf; what it costs is that a throttled create surfaces for reconciliation
instead of riding out the rate limit — the right trade when the alternative is a
duplicate nobody was told about.

**Coverage.** Flipping both flags broke no test, which is the finding underneath
the finding: this behaviour was untested. Two new tests hold a 429 at each create
and assert exactly one POST reaches it plus an UNCONFIRMED outcome carrying the
reconcilable partial; verified to report 4 POSTs when the flag is set back. The
doc comment on `TestCreateCampaign_429OnAuthoringIssuesExactlyOneRequest` asserted
the disproven claim in prose and is corrected to name server-side convergence as
the only qualifying kind. `redactURLForError` and the IPv6 snapshot runs likewise
gained tests that fail against the previous code.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the
redaction line and the retry-eligibility rule),
`docs/knowledge/code/internal-dispatch.md` (the bracketed-host exception to the run
terminators). `docs/api-catalog.md` records that validation errors now redact to
scheme+host.

Refs: LFXV2-2665
