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

NATS **core**, not JetStream: the Query Service re-indexes on every write, so a dropped message
self-heals on the next update.

See [internal/infrastructure/indexer](../../../internal/infrastructure/indexer).
