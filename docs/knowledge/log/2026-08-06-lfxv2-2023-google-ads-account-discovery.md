# 2026-08-06 — LFXV2-2023 Google Ads account discovery

**Update** — Added `AccountLister`, a third optional dispatcher capability alongside
`StatusToggler` and `MetricsReader`, with a Google Ads implementation and the
`GET /projects/{projectId}/connection-google-ads/accounts` endpoint behind it.

The endpoint enumerates the ad accounts reachable upstream with the connection's stored
credential, so configuring a connection does not require pasting a customer ID by hand. It is a
live read and persists nothing.

`docs/api-catalog.md` carried a note reading "There are no `/{provider}/accounts` listing
endpoints." That note was about *stored connections* — a project holds at most one connection per
provider, so there is no collection to list — but as written it read as forbidding this path
shape outright. Scoped it to the Monitoring surface and spelled out the distinction: discovering
accounts that exist **at the provider** is not listing anything this service stores.

`Client.ListAccessibleCustomers` initially hand-rolled its own transport — URL construction,
body bounding, and `apiError`/`transportError` classification duplicated from `doRequest`, about
55 lines. The endpoint being account-agnostic (no `customers/{id}` segment) does not actually
require any of that: `doRequest` builds the same URL, and a nil body means it omits
`Content-Type`. Routed it through `doRequest` with `idempotent=true`, which also picks up 429
retry for free — safe here because the call is a pure read. Every pre-existing test passed
unchanged, which is what established the refactor was behaviour-preserving.

The call was also being sent as **POST**, copied from the `:search` and `:mutate` custom
methods. `CustomerService.ListAccessibleCustomers` is bound to **GET** and takes no request
body at all, so this would have failed against the real API on the first live call. Switched
to GET and pinned the verb in a test, since the POST habit is the natural thing to reach for
in this package.

Two more corrections that the endpoint's first end-to-end use would have hit:

- `ListAccounts` built its result with `var accounts []model.AccessibleAccount`, so a
  credential reaching **zero** ad accounts returned nil. `Orchestrator.ReadAccounts` treats
  a nil result as a lister contract violation and converts it to a 503 — reporting the
  platform as down for a perfectly ordinary empty answer. Pre-allocating with
  `make(..., 0, len(customers))` fixes it; the test fails if the allocation is removed.
- A project with **no stored connection** also surfaced as 503. `credsSource.resolve` built
  a fresh error for the not-found case, dropping the `domain.ErrNotFound` sentinel, so the
  handler could not distinguish "you have not configured this yet" from "the platform call
  failed". `resolve` now wraps the sentinel (the dispatch paths only consult
  `NoUpstreamCreate`, so nothing else changes) and the handler maps it to 404.

Finally, the route was mounted by Goa but **unreachable through the gateway**: the chart's
HTTPRoute regex admits `connection-{provider}` plus only `/test` and `/set-credential`, and
the Heimdall RuleSet rules the same three. `/accounts` was in neither. Because discovery is
Google Ads-only, `connection-google-ads` is now spelled out as its own alternation branch
rather than widening the shared one — folding `/accounts` into the family alternation would
forward a path the RuleSet does not rule, which is the route/rule parity violation
`parity_test.go` exists to catch. Both the accepted (`google-ads`) and rejected
(`linkedin-ads`) rows are pinned there. The ko-embedded OpenAPI copies under
`cmd/campaign-service/kodata/gen/http/` had also not been re-synced after `apigen`.
