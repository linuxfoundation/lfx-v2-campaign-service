# 2026-08-22 — a doc paragraph outlives the behaviour it described

**Fix** — three suppressed review findings on the settings readback, plus a merge repair.
Two of the three were prose that had gone stale against fixes landed EARLIER on this same
branch; the third was a test that asserted the wrong thing. No behaviour changed here.

## A fix that changes a decision must sweep the prose that names the old one

Two commits on this branch turned `googleAdsDateOnly` into a fail-closed helper (nil on an
unparseable value) and made absent provenance refuse `ReadSettings` before the platform call.
Both fixes were correct and both are pinned by tests. What neither swept was the prose that
had described the PREVIOUS behaviour:

- `internal-dispatch.md` still said the helper "passes an unparseable value through WHOLE" —
  and then, twelve lines lower in the SAME paragraph, correctly said it "now enforces the same
  discipline for dates, returning nil". The document contradicted itself; a reader had no way
  to tell which half was current.
- `docs/api-catalog.md` still said **"Unknown provenance is NOT a 409 here, deliberately"**,
  with a rationale for why the endpoint follows the metrics/toggle convention — when the code
  had been changed to do the exact opposite, and deliberately so.

The api-catalog case is the more expensive one. That table is the API contract: it told
consumers to expect a read to proceed where the service now answers 409, so a client's error
handling would have been written against a status the endpoint never returns.

**The rule.** When a fix changes what a code path DECIDES — not merely how it computes — the
change is not finished until every document that names the old decision is re-read. Grep for
the sentinel, the helper name, and the status code. A stale sentence next to a correct one is
worse than no sentence: both read as authoritative, and the reader cannot arbitrate.

## A test may not derive its expectation from the constant under test

`TestOrchestrator_ReadCampaignSettings_BoundsTheCall` asserted only that SOME deadline existed.
The review's remedy — bracket the call and assert the deadline is approximately
`settingsCallTimeout` — was applied, and the mutation **survived**: widening
`settingsCallTimeout` to two minutes moved the expectation along with the code, and the test
stayed green while the bound it exists to pin was defeated.

A bracketing assertion computed from `settingsCallTimeout` can only prove the deadline was
derived from that constant. It cannot prove the constant is still SOUND, because it has no
independent reference to compare against.

The bound that actually matters is external: a synchronous read must finish inside the server's
`constants.DefaultWriteTimeout` (60s), or the platform call outlives the response it was read
for. Asserting `settingsCallTimeout < constants.DefaultWriteTimeout` against that separate
constant kills the mutation.

**The rule.** A test whose expected value is computed from the same symbol the code reads is a
tautology dressed as an assertion. Pin the value against something the mutation cannot move —
a literal, or an independent constant the contract is defined in terms of. When a reviewer
prescribes a remedy, still run the mutation: a suggested fix that cites a real defect is the
most convincing form of an unverified one.

## A clean auto-merge is not a correct merge

`git merge origin/main` reported no conflicts and the tree did not compile. `#158` added a
fourth parameter to `resolveGoogleAdsClient` so the credential follows the campaign's CREATION
customer; this branch's new `ReadSettings` call site — added in parallel, in a region main never
touched — still passed three. Textual non-overlap is exactly why git stayed silent.

Passing `campaign` was the correct repair rather than a compile-appeasing `nil`: `ReadSettings`
is documented as enforcing the account-identity invariant more strictly than its siblings, and
`nil` would have resolved the credential without the creating-account pin the guard below it
then depends on.

**The rule.** A semantic conflict is invisible to a textual merge. Build and run the tests
before believing a clean merge, and repair a broken call site by reading what the new parameter
MEANS — the signature change carries an intent the compiler cannot state.
