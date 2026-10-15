# 2026-10-15 — Every non-Google entry point's credential resolver is pinned

**Update** — #174 (tests only; Google Ads out of scope).

- The resolvers were tested directly, but which one each dispatcher entry point passes was not.
  Hardcoding `d.creds.resolve` into `ToggleStatus`/`ReadMetrics` kept the suite green, and that
  swap makes a pre-cutover campaign unpausable.
- New `internal/dispatch/resolver_identity_test.go` runs a table-driven sweep for LinkedIn,
  Meta, Reddit, X, Microsoft and HubSpot covering 90 entry points: create, discovery, toggle,
  metrics, budget/bid levers, settings, keyword levers, ad sets, audience/keyword insights,
  adoption, probes and account-monitor reads. Each row asserts the resolver identity it uses.
  The identity is read from the scopes a recording `scopedConnReader` is asked for. Two
  credential-less fixtures run with the forced-system flag ON, so resolution fails before any
  client is built and the sweep makes no network calls. Existing-campaign rows also run a
  third fixture, in which the project row decrypts and carries the recorded account. That
  fixture pins the binding to the RECORDED creation account, not just to some non-empty id.
- `TestClassifyResolver_FingerprintsAreDistinct` checks that the five resolvers still produce
  distinct fingerprints.
- Mutation-checked on every dispatcher: switching an existing-campaign call site to
  `d.creds.resolve`, binding `existingResolver("")` or another non-empty id, swapping a `resolveOwned` read to
  `resolve`, or collapsing the `resolveCreds` pass-through in the Meta/LinkedIn/Reddit/HubSpot
  helpers each fails the sweep.
- No entry point was found using the wrong resolver.
- Concept updated: `code/internal-dispatch.md` (forced-primary mode, after the `credsResolver`
  paragraph).
