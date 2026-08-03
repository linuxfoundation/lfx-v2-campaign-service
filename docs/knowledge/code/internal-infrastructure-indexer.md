---
type: "Code Concept"
title: "internal/infrastructure/indexer"
description: "Publishes brief and campaign snapshots to NATS for the platform Query Service, which indexes them into OpenSearch."
resource: "internal/infrastructure/indexer"
---

# internal/infrastructure/indexer

Publishes resource snapshots so the Query Service can serve the lists and revision history this
service deliberately does not implement (architecture **D5**).

## The contract comes from the INDEXER, not the query service

The message is `lfx-v2-indexer-service`'s `LFXTransaction`
(`internal/domain/contracts/transaction.go`, `pkg/types`):

| Field | Notes |
|---|---|
| `action` | `create` / `update` / `delete`. **Required** — a message without it is rejected with "missing or invalid action in message data". |
| `headers` | Authenticated-principal headers, read from the PAYLOAD (not native NATS headers). Marshalled as `{}` here: publishes happen after the write commits, often on a detached shutdown context, so the principal is not reliably available. |
| `data` | The resource snapshot. Deletes pass only the id. |
| `indexing_config` | `object_id` plus the FGA fields. Without it the resource cannot be authorized or found. |

**`object_type` is NOT in the payload.** The indexer derives it from the SUBJECT
(`lfx.index.<object_type>`) — a service can only publish to subjects for its own resource types,
which is how that boundary is enforced.

FGA values come from architecture **D2** (no new object types; only relations on `project`), so
both the access and history checks are `campaign_manager` on `project:<projectId>` — this
service has no read-only audience that would justify separate relations. `public` is always
false: every resource is project-scoped.

## Same-resource publishes are serialized

The indexer does NO version comparison — it overwrites the current document with whatever
arrives last. Two writers committing v2 then v3 can reach `Publish` in the reverse order, and
the index then holds v2 permanently: both writers believe they succeeded, so no later write
repairs it.

`NATSPublisher` therefore holds a per-object-id lock across marshal+publish+flush. Different
resources never contend.

This orders the PUBLISH, not the commit — two writers that commit in one order and call
`Publish` in the other are still mis-ordered. Closing that needs the outbox pattern (tracked
separately); this removes the far more common in-process reordering.

### The mistake worth remembering

An earlier version published a FLAT body copied from `lfx-v2-query-service`'s
`TransactionBodyStub`. That type is the `_source` shape the indexer **produces** after
processing a message — not what a producer sends. Publishing it meant every message was dropped
before indexing, and nothing in this service errored: it looked fully wired and indexed nothing.

When in doubt, read the CONSUMER's contract. `TransactionBodyStub` describes the output.

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
