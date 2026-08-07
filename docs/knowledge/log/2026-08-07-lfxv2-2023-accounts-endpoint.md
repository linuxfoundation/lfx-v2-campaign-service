# 2026-08-07 — The account-discovery endpoint, and why its "no connection" case is a 404

`GET /projects/{projectId}/connection-google-ads/accounts` is a **live, credential-scoped read
of what exists upstream**, not a listing of anything this service stores. That distinction is
the whole reason it can coexist with the catalog's standing rule that there are no
`/{provider}/accounts` listing endpoints: a project still has at most one stored connection per
provider, and this endpoint is how an operator decides which upstream account that single
connection should point at.

**Three failure modes, three different statuses, because they ask the operator to do three
different things.**

- **400** — the platform has no account-discovery capability wired. Like `MetricsReader`, the
  capability is optional per dispatcher; only Google Ads implements it today. Nothing to retry.
- **404** — the project has no stored connection for the provider. This is a SETUP state, not an
  outage, and it is the one worth spelling out: a 503 here would tell the caller to retry
  something that cannot succeed until a connection is created. The orchestrator resolves the
  connection before it reaches the dispatcher precisely so this case is answerable.
- **503** — the provider call itself failed. The only one where retrying is the right response.

The result is never persisted. There is deliberately no write-back of the chosen account from
this path; choosing is a separate `PUT` on the connection, so a read that happens to enumerate
an account never mutates which one is configured.

This PR is the service wiring and the HTTP surface. It sits on the dispatch adapter
(`feat/LFXV2-2023-accounts-dispatch`), which sits on the platform client
(`feat/LFXV2-2023-accounts-platform`). Split into three because the original single PR reached
1944 hand-written lines against a 1000-line cap.
