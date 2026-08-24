# 2026-08-24 — LFXV2-3033 the merge had to restore a dropped provenance return

**Fix** — merging `origin/main` into the client-cache branch conflicted in
`internal/dispatch/reddit.go`, and the two sides disagreed about the function's ARITY,
not just its body. That is the shape of conflict a side-picking resolution resolves
wrongly while still compiling.

On this branch, the reddit client construction had been wrapped in the client cache and
returned `(*reddit.Client, error)`. On `main`, LFXV2-3050 had since split the resolver in
two: `resolveRedditClient` kept the narrow signature for the read-only callers, and a new
`resolveRedditClientWithCreds` returned `(*reddit.Client, *resolved, error)` so `Dispatch`
could stamp `ran_on_system_account` from the credential the client was actually built
from. The conflicting hunk sits inside the WIDER function.

Taking this branch's side verbatim keeps the cache and silently returns a two-value
result from a three-value function — which does not compile, and so would have been
caught. The dangerous resolution is the near miss: keep the cache, add a third return,
and pass `nil` for it because that is what type-checks. `go build` passes, `go vet`
passes, and every reddit campaign is then recorded with unknown provenance — the exact
column LFXV2-3050 was opened to populate. The repaired arms return `res`, the value the
sibling call sites on `main` pass, rather than a `nil` chosen for compiling.

The base side of the conflict is also a live hazard here. This repo emits `diff3` output,
so the hunk carried FOUR markers, and the `|||||||` base section is a complete,
compilable copy of the pre-cache construction. A resolver splitting on three markers
re-appends it as live code, producing a second unreachable return path that builds
cleanly.

**Verification** — symbol arithmetic over `^func ` on the three versions: ours 14,
theirs 15, merged 15, with the merged set a strict subset of the union and nothing
present that neither side contributed. `Dispatch` resolved to `main`'s named-return
variant, which is what the provenance `defer` requires.

Two mutations, each compiling and each reverted:

- Replacing the `buildOnce` call with a direct construction fails
  `TestClientCache_RedditReusesClientAndToken` and
  `TestClientCache_RedditColdKeyConcurrentBuildsAreCoalesced` — the cache this branch
  exists to add is genuinely covered.
- Returning `nil` instead of `res` on the cached success path fails
  `TestReddit_DispatchStampsProvenanceEndToEnd` and
  `TestAllDispatchers_StampProvenanceOnEveryCampaignReturn`. That is the plausible wrong
  resolution above, and `main`'s tests kill it — the repair is load-bearing rather than
  cosmetic.

The token-invalidation guard this branch adds for reddit, microsoft and google ads was
untouched by the merge: `git diff HEAD` reports no change to the three
`internal/platform/*/client.go` files, so the open review finding about invalidating only
the token the rejected request presented remains open against unchanged code, neither
papered over nor silently resolved by the merge.
