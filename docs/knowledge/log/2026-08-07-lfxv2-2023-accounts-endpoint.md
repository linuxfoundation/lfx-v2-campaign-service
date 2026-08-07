# 2026-08-07 — The account-discovery endpoint, and why its "no connection" case is a 404

**Update** — New endpoint on PR #85 (`design/connection.go`, `internal/service/`,
`internal/container/container.go`, and the chart route/ruleset templates).

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
  something that cannot succeed until a connection is created. The DISPATCHER owns connection
  resolution — `GoogleAdsDispatcher.resolveGoogleAdsDiscoveryClient` reads the stored
  connection; `Orchestrator.ReadAccounts` only type-asserts `AccountLister` and delegates. What
  makes the case answerable is therefore not where resolution happens but that the dispatcher
  wraps with `%w`, so `domain.ErrNotFound` survives up to the handler intact. Getting this
  attribution right matters for the next provider: it tells the implementer that resolution is
  their job, and that flattening the error is what breaks the 404.
- **503** — the provider call itself failed. The only one where retrying is the right response.

The result is never persisted. There is deliberately no write-back of the chosen account from
this path; choosing is a separate `PUT` on the connection, so a read that happens to enumerate
an account never mutates which one is configured.

This PR is the service wiring and the HTTP surface. It sits on the dispatch adapter
(`feat/LFXV2-2023-accounts-dispatch`), which sits on the platform client
(`feat/LFXV2-2023-accounts-platform`). Split into three because the original single PR reached
1944 hand-written lines against a 1000-line cap.

## Review pass — the retry branch had no test

The three-status mapping above was tested at two of its three arms: 400 (no capability
wired) and 404 (no stored connection), plus a 503 for the dependency-unavailable case —
no orchestrator wired at all, which returns before the switch is ever reached. The arm
that actually fires in production, a wired dispatcher whose provider call failed, had
no coverage. It is also the only one of the three that tells the caller to **retry**,
so getting it wrong is the expensive direction.

`TestListGoogleAdsAccounts_ProviderFailureIs503` covers it, and pins the boundary rather
than the happy path. Its second subtest returns an error whose message reads
`"customer 123 not found upstream"` but wraps no sentinel, and asserts **503, not 404**.
The 404 branch is gated on `errors.Is(aerr, domain.ErrNotFound)`; if that ever loosened
into string matching, a transient provider failure would be reported as permanent
client-side state and the UI would stop retrying a call that would have succeeded.
Revert-verified by loosening exactly that gate — the subtest fails, the plain-failure
one still passes, which is what makes it a boundary test rather than a duplicate.

It also asserts the upstream text does not appear in the returned message. A provider
error can carry customer ids and account state the caller has no relation to; it is
logged with the project id and answered generically.
