# 2026-08-22 — LFXV2-3319 pin the AccountLister roster, stop correcting it

**Fix** — this PR gave X both account-eligibility halves and, in doing so, falsified two live
claims that single out Microsoft, plus a third in a knowledge doc:

- `internal/bootstrap/sysacct.go:297` said X's exclusion put it in *"the same
  position Microsoft is in"*, as though Microsoft were the only other provider
  holding both halves while excluded. X now holds both too, so the sentence
  names one of at least two and reads as an exhaustive pair.
- `internal/bootstrap/sysacct_test.go:511-513` said *"Microsoft is the one
  member worth calling out… it holds both halves and is still excluded."*
  Directly false as of this PR.
- `docs/knowledge/kubernetes/ruleset.md:23-29` still listed four `AccountLister`
  providers and named `twitter-ads` among those *"whose clients have no
  `ListAdAccounts`"*. This PR updated `httproute.md` and not its companion.

**The structural point, which is the actual finding.** The comment above
`accountDiscoveryProviders` already warned that *"an enumeration of members goes
stale silently — this comment described a Google/Meta-only world for two tickets
after that stopped being true"*, and the test comment recorded that it had been
**corrected three times, each correction falsified by the next ticket**. This
would have been the fourth correction. A fourth correction buys one ticket.

So the fix is a parity test, not a correction. Both options were evaluated:

**Option 1 — derive the prose from the map at runtime. Rejected, on two grounds,
the second decisive.** First, the prose that went stale is not in the map's
package: it is a Markdown knowledge doc, a Goa design comment and a test
comment. Runtime derivation cannot reach any of them; it would fix the one
restatement already adjacent to the map and miss all three that actually
drifted. Second — and this is the part that settles it — **there is no single
map to derive from.** `accountDiscoveryProviders` is `{GoogleAds, Meta}`;
`AccountLister` is `{GoogleAds, Meta, LinkedIn, Microsoft, X}`. They are
different sets measuring different things. The bootstrap map requires BOTH
eligibility halves, and the second half is *"`Dispatch` itself calls the
validator that tags `ErrAccountNotSelected`"* — a call-graph property. Measured:
**every** dispatcher, Reddit included, mentions that sentinel (3, 2, 3, 3, 5 and
3 times respectively), so no grep, symbol table or reflection distinguishes
LinkedIn (tagged in a resolver `Dispatch` never calls) from X (whose `Dispatch`
calls the tagging validator itself). A derived sentence would have to hardcode
the very judgement that keeps going stale.

**Option 2 — a parity test that fails when prose and code disagree. Chosen.**
The first half IS mechanically checkable, and the first half is exactly what
LFXV2-3319 moved. `TestAccountListerProseMatchesTheInterface` derives the roster
from `service.AccountLister` by type assertion — nil dependencies, since a method
set is a compile-time property and nothing is called — and holds the HTTPRoute
template and `ruleset.md` to it. It follows the precedent already in this repo:
`charts/parity_test.go` couples the route regex to the Heimdall RuleSet, and this
closes the remaining side, because chart-vs-chart parity cannot notice that both
sides agree on a set the code has moved past.

The split is the design: **pin what a compiler can see, and stop restating what
it cannot.** The second half stays prose because it is a human judgement, and
`TestAccountDiscoveryProvidersIsASubsetOfAccountListers` pins only the invariant
that survives churn — every member of the bootstrap map holds the first half.
It deliberately does **not** assert equality. The sets are unequal on purpose
(Microsoft and X hold both halves and are still excluded, a sequencing decision),
and asserting equality would turn that judgement into a test failure every time
discovery lands for a provider — which is the exact pressure that produced three
successive hand-corrections.

**Mutation-verified, both a false-negative and a vacuity check.**

Reverting `ruleset.md` to its stale wording fails the test with one precise
finding:

    docs/knowledge/kubernetes/ruleset.md lists "twitter-ads" among "the providers
    with neither ... whose clients have no ListAdAccounts", but twitter-ads
    implements service.AccountLister.

Exactly one provider is named. An earlier draft matched a ±400-character window
around the phrase and flagged all five, because the surrounding paragraph also
names the providers that DO have discovery — a check that fires on correct prose
is one that gets deleted. The assertion binds to the parenthetical group
`the providers with neither (...)` instead.

Second mutation, against the failure mode a doc-reading test is most prone to:
replacing that parenthetical with "and the remaining providers —" removes the
anchor entirely. The test FAILS rather than passing vacuously:

    docs/knowledge/kubernetes/ruleset.md no longer contains the "the providers
    with neither (...)" enumeration this test binds to ... an unbindable check is
    one that passes forever.

Dropping `TwitterDispatcher.ListAccounts` was tried as a third mutation and is
**not** reportable: it does not compile, because four existing tests in
`account_discovery_test.go` call the method directly. Recorded here so the next
reader does not retry it — the interface half is already held by those tests;
what was unheld, and is now held, is the agreement between the interface and the
prose.

`sysacct.go` and `sysacct_test.go` are corrected too, but rewritten as RULES
rather than rosters: they now state that holding both halves is not sufficient
for membership **without naming which providers that covers**, since naming them
is the thing that has gone stale four times, and they point at the two tests.
