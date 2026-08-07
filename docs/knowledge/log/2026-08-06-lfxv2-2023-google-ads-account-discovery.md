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
