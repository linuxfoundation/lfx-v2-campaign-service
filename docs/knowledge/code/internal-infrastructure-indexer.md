---
type: "Code Concept"
title: "internal/infrastructure/indexer"
description: "Publishes brief and campaign snapshots to NATS for the platform Query Service, which indexes them into OpenSearch."
resource: "internal/infrastructure/indexer"
---

# internal/infrastructure/indexer

Publishes resource snapshots so the Query Service can serve the lists and revision history this
service deliberately does not implement (architecture **D5**).

## The contract is derived, not invented

Every field was traced to an existing platform source rather than chosen:

| Element | Value | Derived from |
|---|---|---|
| Subject | `lfx.index.<object_type>` | the four subjects already in use (`committee_document`, `project_document`, `individual_vote`, `vote_response`) |
| Body | `TransactionBodyStub` fields | `lfx-v2-query-service/internal/domain/model/resource.go`, whose comment marks it as the `_source` shape |
| `access_check_relation` | `campaign_manager` | architecture **D2** and `charts/…/ruleset.yaml`, which gates every route on it |
| `access_check_object` | `project:<projectId>` | same ruleset — D2 forbids new FGA object types |
| `public` | always `false` | every resource is project-scoped |

Two consequences worth knowing:

- **History check == access check.** D2 gives this service one relation for reads AND writes
  (there is no read-only campaigns audience), so the two checks are deliberately identical.
- **`object_type` and `public` are read straight out of `_source`** by the Query Service's
  searcher. A wrong value there indexes cleanly and then matches nothing — a failure that looks
  exactly like indexing being broken.

## Best-effort by design

`Publisher.Publish` returns no error, and the call sites do not check one. The database is the
source of truth; a publish failure costs discoverability until the resource's next write, never
correctness. Making indexing able to fail a write would turn a NATS outage into a
campaign-service outage.

The same reasoning applies at startup: an unreachable broker logs and returns a `Noop` rather
than blocking boot. `Noop` also stands in when `NATS_URL` is empty, so the five write paths
publish unconditionally instead of nil-checking at each one.

NATS **core**, not JetStream — which is **at-most-once**. A dropped message is lost.

**This does not self-heal**, and an earlier version of this document wrongly said it did. A
dropped document is repaired only by a SUBSEQUENT write, and several writes have no successor:
archiving a brief is terminal, and a created-then-never-edited campaign may never be written
again. Since the Query Service serves lists and history FROM the index, the result is
user-visible staleness, not a cache miss.

Bounding the publish flush narrows the window but cannot close it: the process can die between
the commit and the publish regardless. Closing it properly requires delivery to be recoverable
independently of the write — a transactional outbox with a relay, or a periodic
database-to-index reconciliation sweep. That is **not implemented**; it is tracked as a known
gap rather than claimed as handled.

What core NATS does buy is that indexing can never fail a write — the property the service
actually depends on, since the database is the source of truth.

## Wiring: one publisher, injected everywhere

`SetIndexer` is **opt-in** (so the ~40 existing test call sites default to `Noop`), which makes a
missed injection silent — the service compiles, boots, serves traffic and indexes *nothing*. That
is not hypothetical: the first cut of this feature injected only on the 503-mode path, so a
**healthy** startup published nothing at all.

Two guarantees prevent a repeat:

- The publisher is built **once** in `NewContainer`, before any wiring branch, and held on the
  `Container`. All three `BriefService` paths (no-database, live fast path, 503-mode + retry) and
  both `Orchestrator` paths go through `newBriefService` / `newOrchestrator`, which inject it.
- `BriefService.IndexerIsNoop` / `Orchestrator.IndexerIsNoop` exist **only** so the container's
  wiring tests can assert every path got a real publisher.

**Both** types need it: `BriefService` publishes brief writes and campaign *updates*, but campaign
**creates** are persisted by `Orchestrator.dispatchOne`. An orchestrator left on `Noop` leaves every
newly created campaign unsearchable until some later update happens to republish it.

Retained **partial** orphans are indexed too, not just clean successes — those are precisely the
rows an operator must be able to find in order to reconcile them. Both publish on the *detached*
`persistCtx`, so a write completing during shutdown grace still reaches the index.

`NATS_URL` set to the empty string disables publishing outright. That switch is only reachable
because config resolves it with `envOrDefaultUnlessSet` (`os.LookupEnv`): the ordinary
`envOrDefault` treats empty as absent and would substitute the in-cluster default, making the
documented switch a no-op.

See [internal/infrastructure/indexer](../../../internal/infrastructure/indexer).
