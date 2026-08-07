# 2026-08-07 — Account discovery: the configured MCC is not selectable, and the call is bounded

**Update** — Closed the suppressed Copilot findings on PR #82
(`internal/platform/googleads/client.go`, `accounts_test.go`,
`internal/service/orchestrator_test.go`, and the Google Ads concept).

The manager filter only covered half the input. `listManagerClients` drops rows where
`customer_client.manager` is true, which handles the expansion — but the FLAT list from
`customers:listAccessibleCustomers` was merged in unfiltered, and it carries no `manager`
flag at all. A resource name alone cannot tell a manager from an ad account. **On an MCC
credential the flat list is usually the manager itself**, which is exactly the shape this
endpoint exists to serve, so the configured MCC stayed in the picker as a selectable
choice — and picking it produces a connection that fails at the first campaign create,
far from where the choice was made.

The one manager identifiable without extra metadata is the configured
`login_customer_id`, so exactly that resource name is now dropped from the direct
results. Any OTHER manager in the flat list survives: there is no field here to recognise
it by, and a per-row round-trip to find out would cost more than it saves on a list this
short. The hierarchy test previously asserted the manager REMAINED selectable; it now
asserts the opposite and pins the reason, which is that the two managers reach the merge
by different paths — the sub-manager through the expansion's flag, the configured MCC
through its own id.

`ReadAccounts`' 20-second bound had no test. The bound matters more here than the absence
of one usually does: account discovery is SYNCHRONOUS, made while an HTTP request is held
open, and it reaches an external provider twice on an MCC credential. **An unbounded call
there does not fail, it hangs** — the handler's context may carry no deadline at all, and
nothing else in the path imposes one, so a provider that stops responding pins a request
goroutine indefinitely. `TestOrchestrator_ReadAccountsBoundsThePlatformCall` hands
`ReadAccounts` a deadline-free `context.Background()` and asserts the lister received one
derived from `accountsCallTimeout`, mirroring the metrics path's existing assertion. The
deadline-free caller context is the point: it proves the bound comes from the
orchestrator rather than being inherited.

The other findings in the same review were already satisfied on this branch and needed no
change: the kodata OpenAPI copies carry the account route, both chart matchers
(`httproute.yaml` and `ruleset.yaml`) list `/accounts`, `docs/api-catalog.md` documents
the 404 and reconciles its "no account-listing endpoints" note with this live read, the
missing-connection sentinel maps to `conn.NotFoundError`, and the discovery path no
longer requires a stored account id.
