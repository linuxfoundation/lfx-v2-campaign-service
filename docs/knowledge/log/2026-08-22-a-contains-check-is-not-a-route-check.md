# 2026-08-22 — a `Contains(slug)` check is not a check that the route exists

**Fix** — review follow-up on the parity test added earlier in this PR
([2026-08-22-LFXV2-3319-pin-the-roster-stop-correcting-it](2026-08-22-LFXV2-3319-pin-the-roster-stop-correcting-it.md);
that fragment's reasoning about *why* a parity test replaces a fourth prose correction is
intact and unchanged — only the chart-side assertion described here was wrong).

## The defect

`TestAccountListerProseMatchesTheInterface` asserted the chart side with

```go
if !strings.Contains(route, slug) { ... "its /accounts path is not forwarded" ... }
```

against the raw `httproute.yaml`. The error message claims a property about **`/accounts`**,
but the predicate never mentions `/accounts`. Every provider slug already appears in that
file two or three other times:

- in the per-provider `(google-ads|linkedin-ads|meta-ads|reddit-ads|twitter-ads)/metrics`
  alternation, which is a *different* endpoint family; and
- in the surrounding explanatory comments, which spell the slugs out in prose.

So the discovery branch — `connection-(…|twitter-ads)(/(test|set-credential|accounts))?` —
could be edited away entirely and `Contains` stayed true on the leftovers.

## Confirmed by mutation, not by reading

Deleting `twitter-ads` from the `connection-(...)` discovery alternation, so that
`/projects/{p}/connection-twitter-ads/accounts` is no longer forwarded at all, left the test
**PASS**. `TwitterDispatcher` still implements `AccountLister` throughout; the endpoint was
simply unreachable, which is the exact condition the test was written to pin.

The chart-side suite did not cover the gap either, though it takes one more step to see it.
`charts/parity_test.go` compares the route against the RuleSet, so a **one-sided** deletion
fails it loudly. But delete the path from **both** sides and remove the one curated table row
`{"/projects/p1/connection-twitter-ads/accounts", true}`, and the whole charts package returns
**ok**: route and RuleSet agree with each other about a capability the code still implements.
Chart-vs-chart parity cannot detect that both sides have drifted away from the interface
together — which is precisely the seam this test was supposed to own.

## The fix

Compile the matchers and probe a **concrete path**, one per `AccountLister`:

- `routeRegexp` extracts the single `^/projects/` `RegularExpression` value and compiles it.
  The value carries no Helm templating, so it compiles from the raw template and the test
  still runs without `helm` on PATH; if templating is ever introduced there, the helper
  `t.Fatal`s rather than silently matching a literal `{{`.
- `ruleSetMatchers` extracts the `- path:` entries of the `project-api` rule **only** — the
  same scoping `charts/parity_test.go` uses, because a path moved into an `allow_all` or
  differently-scoped rule must not count as authorized — and compiles each Traefik pattern.
- For each provider implementing `AccountLister`, `/projects/p1/connection-<slug>/accounts`
  must be forwarded by the route **and** authorized by the RuleSet. Heimdall is default-deny,
  so an unruled path is unreachable even when the route forwards it; both sides are therefore
  independently load-bearing and are asserted separately.
- The **negative** direction is asserted too: a provider that does *not* implement
  `AccountLister` must have neither route nor rule for its `/accounts`, or the chart admits a
  path the service answers with a 400 by construction. This is what keeps `reddit-ads` honest
  if the discovery branch is ever collapsed back into the shared alternation early.

## Mutation results after the fix

Each of these was applied to a **compiling** tree and the diff inspected to confirm the edit
landed on the intended clause before the test was run:

| mutation | before fix | after fix |
| --- | --- | --- |
| route: drop `/accounts` for `twitter-ads` | PASS | **FAIL** — names the route arm |
| RuleSet: drop `/accounts` path for `twitter-ads` | PASS | **FAIL** — names the RuleSet arm |
| both sides together (+ curated row removed) | PASS everywhere | **FAIL** — both arms |
| route: widen shared branch so `reddit-ads` gains `/accounts` | PASS | **FAIL** — negative arm |

The RuleSet mutation was additionally run with the route **restored**, to confirm that arm
fails on its own rather than riding on the route failure.

## The generalisation

A substring check over a structured matcher tests the alphabet, not the grammar. When the
token you search for legitimately appears elsewhere in the same file — another endpoint
family, or the prose comment explaining the rule — `Contains` is satisfied by the *neighbours*
of the thing you meant to pin, and it degrades to a spell-check on a word that is guaranteed
present. **Compile the matcher and ask it about a concrete input.** The error message is the
tell: if it asserts something (`/accounts`) that the predicate never names, the predicate is
measuring something else.

Corollary, worth stating because the charts suite looked like coverage: two artifacts checked
against **each other** cannot catch drift that moves both of them the same way. A parity test
needs at least one leg anchored to something outside the pair — here, the Go type system.
