# 2026-08-07 — Account discovery: why the call is bounded, and why it is a GET

**Update** — Both facts below are enforced in code today (`internal/platform/googleads/client.go`,
`internal/service/orchestrator.go` and its bound test). Neither had a log entry: the fragments
that carried them lived only on the superseded `feat/LFXV2-2023-account-discovery` branch, which
was closed once its content landed through `main` and the dispatch branch. Recording them here so
the reasoning survives the branch that produced it.

## The 20-second bound is not boilerplate

`ReadAccounts` derives a context from `accountsCallTimeout` rather than passing the caller's
through. The bound matters more here than on most reads:

- discovery is **synchronous** — it runs while an HTTP request is held open;
- on an MCC credential it reaches the provider **twice** (the flat list, then the
  `customer_client` expansion);
- the handler's context may carry **no deadline at all**, and nothing else on the path imposes
  one.

So an unbounded call there does not fail — **it hangs**, pinning a request goroutine for as long
as the provider stays silent. `TestOrchestrator_ReadAccountsBoundsThePlatformCall` hands
`ReadAccounts` a deadline-free `context.Background()` and asserts the lister received one derived
from `accountsCallTimeout`. The deadline-free caller context is the point of the test: it proves
the bound originates in the orchestrator instead of being inherited from whatever called it.

## `customers:listAccessibleCustomers` is bound to GET

It was first written as a **POST**, copied from the `:search` and `:mutate` custom methods that
dominate this package. `CustomerService.ListAccessibleCustomers` is bound to **GET** and takes no
request body at all, so the POST form would have failed against the real API on the first live
call — and only on a live call, since nothing in the fake exercised the verb.

The verb is now pinned by a test. That test is worth keeping specifically because POST is the
natural thing to reach for in this file: every neighbouring call is one.

**The general lesson: a verb copied from a neighbouring call is an untested assumption.** Custom
methods (`:search`, `:mutate`) and standard methods do not share a binding, and a fake that
ignores the method cannot tell you which one you picked.
