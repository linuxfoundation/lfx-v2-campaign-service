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
| `action` | **Past tense only**: `created` / `updated` / `deleted`. The V2 validator REJECTS the imperative forms, so `create` would discard every message. |
| `headers` | Must carry a NON-EMPTY lower-case `authorization`. `validateV2Headers` drops any V2 message without it, so the caller's bearer token is threaded from the request — including through async dispatch, where it is captured at `Orchestrator.Start`. |
| `data` | The resource snapshot for created/updated. For **deleted** it must be the bare object-id STRING — the indexer type-asserts it and rejects an object with "expected string", so a document there means the resource is never removed from search. |
| `indexing_config` | `object_id` plus the FGA fields. Without it the resource cannot be authorized or found. Also carries the SEARCH metadata: the Query Service applies `name=` against top-level `name_and_aliases` and `parent=` against `parent_refs` — a value nested in `data` is NEVER consulted, so a resource with empty name metadata indexes cleanly and is then unfindable by name. Briefs publish their event slug; campaigns their campaign name. |

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
the commit and the publish regardless.

## The outbox closes it

**The drain CLAIMS its batch** with `SELECT ... FOR UPDATE SKIP LOCKED`, holding the row locks
across the publish and the retire (the publish is passed down as a callback for exactly this
reason). An unclaimed read let every replica load the same batch, so a slow pod could publish an
earlier `updated` after a faster one had already published the later `deleted` — resurrecting an
archived document ACROSS replicas, which rolling deploys make routine.

**Exclusivity alone is not enough.** `SKIP LOCKED` will skip an older LOCKED row for object X and
hand a second pod the NEWER row for the same X, publishing an update before its create. So the
claim also carries a `NOT EXISTS` predecessor check: a row is claimable only when no older
pending row exists for the same `(object_type, object_id)`. That means at most ONE message per
resource per pass, and a failed delivery blocks only its OWN resource — publishing past it would
reorder that resource's history. Verified against a live PostgreSQL 16: with one pod holding
`b1`'s create, a concurrent pod claimed ZERO rows rather than `b1`'s update.

**Ordering is by `id`, never `created_at`.** `created_at` defaults to `now()`, which is
TRANSACTION-START time in PostgreSQL: a transaction that began earlier but wrote later gets an
EARLIER timestamp, so sorting by it can invert the committed order of two mutations. `id` is a
`BIGSERIAL` assigned at INSERT and has no such inversion. The partial index is keyed
`(object_type, object_id, id)` to serve both the predecessor check and the ordering.

Because a pass takes one row per resource, `drain` LOOPS while it makes progress (bounded by a
pass cap and the pass deadline) — otherwise a queued create+update+delete would need three ticks
and take 45s to drain a backlog that is ready immediately.

**Campaign writes co-commit too.** Campaign creation is ASYNC — the dispatch runs on the
orchestrator's root context, long after the request returned — so publishing with the JWT
captured at `Start` could fail on an EXPIRED token, and with no outbox row there was nothing to
retry: the campaign stayed permanently unsearchable.

**EVERY brief mutation goes through the outbox** — create, replace, approve AND archive — not
just the terminal archive. A direct post-commit publish cannot be ordered against an outbox
replay: a replace could commit, stall before publishing, and land its update AFTER the archive
had been replayed and its row retired, putting a deleted brief back in the index with no pending
tombstone left to repair it. The publisher's per-object lock cannot prevent this — it is
process-local and orders calls only as they arrive, which says nothing about a stalled replica.
Routing every write through the table gives each brief ONE ordered sequence, which is also what
makes the ordering correct ACROSS replicas.

**Published rows are PRUNED after a retention window** (7 days), a few thousand per relay pass.
Every brief and campaign mutation writes a full JSONB payload and nothing else ever deletes one,
so without this the table, its backups, and the vacuum workload grow until storage runs out — and
the partial pending index stays small either way, which is exactly why the growth would go
unnoticed. PENDING rows are NEVER pruned, at any age. They are undelivered work and this service has no
full-reindex path, so discarding one is UNRECOVERABLE — and the cases that matter are exactly the
ones with no later write to repair them (a terminal brief archive, a created-then-never-edited
campaign). An age-based sweep cannot tell "the indexer has been down for a month" from "this
message is obsolete", and guessing wrong loses data permanently.

Unbounded growth is therefore prevented at the SOURCE: when indexing is DELIBERATELY disabled
(`NATS_URL=""` → the Noop publisher) the payload builder is nil and no row is written at all —
the only place that distinction can safely be made, since the relay cannot tell "disabled
forever" from "broker down for an hour". A missing CREDENTIAL is NOT treated this way: that is a
provisioning gap, the rows are real work, and the relay drains them once the token lands. Pruning runs
AFTER the drain (delivery must not queue behind housekeeping) and a prune failure is logged and
dropped: it costs disk, never correctness.

`index_outbox` (migration 000008) holds a fully-marshalled message written in the SAME
transaction as its resource, so it commits if and only if the resource does. `indexer.Relay`
drains it every 15s and at startup — the likeliest reason rows are pending is that this pod's
predecessor died mid-publish.

- **No credential is ever stored.** The row carries an EMPTY authorization header and the relay
  stamps a service credential (`INDEXER_SERVICE_TOKEN`, wired in the chart as an `optional`
  `secretKeyRef` on `lfx-v2-campaign-service-secrets`; optional so a cluster missing the key
  still starts, with the relay idling rather than draining) at publish time. The table is JSONB
  retained for audit with no pruning, so writing the caller's JWT would persist a live
  credential indefinitely.
- **A publisher that did not send must not retire rows.** `Noop.PublishRaw` therefore reports
  FAILURE — otherwise a pod started with indexing disabled would silently drain every pending
  message as delivered, permanently defeating recovery for messages that never left the process.
- **A FLUSH IS NOT AN ACK.** `PublishRaw` uses NATS request/reply, not publish-and-flush: a flush
  only confirms the bytes reached the BROKER. The indexer subscribes to `lfx.index.*` with reply
  support and answers `"OK"` on success or `"ERROR: ..."` on any envelope/config/data rejection
  (`IndexingMessageHandler.HandleWithReply`). Only a literal `OK` retires the row; a rejection or
  a missing responder leaves it PENDING with the reason recorded on it. Treating a flush as
  delivery silently retired rows for messages the indexer had REFUSED — the same drop the outbox
  exists to prevent, but harder to see, because every row looked delivered.
- **Nor may a publisher that never FLUSHED.** `conn.Publish` only buffers, so `PublishRaw`
  returns the context error when the context has already ended rather than reporting a delivery
  it never confirmed. Checked FIRST — before the connection guard and the per-object lock —
  because `flushBudget` consults only the DEADLINE, so a context cancelled without one would
  otherwise report a full budget and flush anyway.
- **Nor may a publisher whose message will be REJECTED.** With no `INDEXER_SERVICE_TOKEN` the
  stamp would write an empty authorization header: NATS accepts the publish, so the row is
  retired, while `validateV2Headers` drops the message at the far end. `Relay.drain` therefore
  skips the whole pass (warning once) rather than draining the outbox into a black hole. Rows
  stay pending and publish once the token is configured.
- The payload is FROZEN at write time. The relay never re-derives it, so a later contract
  change cannot alter the meaning of a message enqueued under the old one.
- A publish that succeeds but fails to retire its row REPUBLISHES next pass. Safe: the indexer
  overwrites by object id, so a duplicate is a no-op — the right trade against dropping it.
- The stamp decodes into a MAP, not a `Transaction`: `objectType` is unexported and never
  serialized (the indexer derives it from the subject), so a struct round-trip would silently
  lose it. The relay routes from the outbox ROW instead.
- `PublishRaw` takes the same per-object lock as `Publish`, so a replayed message cannot
  interleave with a live one for the same resource.
- Wired for **archiving a brief** — the terminal write with no "next write" to repair it, which
  is the case that motivated this. The other write paths still publish directly; moving them
  onto the outbox is mechanical follow-up now that the machinery exists.

Publishes are also correct when they succeed, bounded so they cannot overrun shutdown, and
serialized per resource so a late message cannot overwrite a newer one.

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
