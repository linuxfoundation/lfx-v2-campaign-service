# Log

## 2026-08-04

**Fix** — Renumbered `000015_index_outbox_lease` → `000011_index_outbox_lease`. It COLLIDED with
`000015_drop_campaigns_full_unique_platform` on `feat/LFXV2-campaign-delete` (PR #64): two
different migration files claiming the same version in two open PRs.

golang-migrate applies one and silently skips the other — no error, the schema just drifts. And
no CI check on either PR could catch it: a uniqueness test globs only its own branch's migrations
directory, so it is green on both PRs and fails only on whichever merges second. The lease
migration was also numbered above `000011`–`000014`, which nothing owned, so the version had no
reason to be 15 in the first place.

Added the numbering guards this needed: `TestMigrations_UniqueNumbering` (duplicate versions),
`TestMigrations_NoVersionGaps` (a version numbered above ones that do not exist yet — if this tree
deploys first, the migrations later filling the gap are skipped permanently), and
`TestMigrations_AllowedVersionGapsAreStillOpen` (an `allowedVersionGaps` entry becomes stale once
its sibling lands, so it must be deleted rather than left to mask a future genuine gap). The
remaining `000008`–`000009` gap is recorded there as the merge-ordering obligation it is: PR #59
must merge before this branch. Each test verified by violating what it guards.

Verified on PostgreSQL 16.10: the chain applies ascending (`000001`–`000007`, `000010`, `000011`)
with the lease columns and `idx_index_outbox_claimable` present and `idx_index_outbox_pending`
replaced, and `000011`'s down migration reverses it.

**Update** — Took the NATS publish OUT of the outbox claim transaction (LFXV2-2814, PR #60). The
drain held one transaction across claim, publish and retire, so a pool connection stayed checked
out for up to the 30s pass budget while the relay waited on the indexer. With a small pool that
blocks every brief/campaign write and the readiness query — the broker degradation this table
exists to isolate becomes a service outage — and `pgxpool.Close` at shutdown waits on that
connection, defeating the bounded `Relay.Stop`. A drain is now three short transactions with the
publish in none of them: claim and commit, publish, then settle each outcome separately.

Removing the long transaction removed what carried exclusivity, since `FOR UPDATE SKIP LOCKED`
locks end at commit. Migration `000011` adds `leased_until`/`leased_by`, stamped in the SAME
statement that selects the rows — splitting the select and the update would leave a window where
two pods both read an unleased row. A row is claimable only when its lease is absent or expired;
the expiry (not a permanent flag) is what lets a crashed pod's rows recover instead of wedging
their resource forever. `leaseDuration` is 60s, which must exceed the pass budget plus the settle
budget or a lease dies mid-publish and a peer republishes behind the pod still working —
`TestClaimStampsALeaseThatOutlastsAPass` pins that, mirroring `relayPassTimeout` locally because
this package must not import `indexer`. Both settle paths are guarded on still holding the lease,
so a pod whose lease expired cannot retire a row or corrupt the new owner's backoff accounting;
settle also runs on a context detached from the pass, because at shutdown the publish may already
have succeeded. The predecessor check deliberately does NOT filter on the lease: an older leased
row is in-flight, not absent, and must still block its successor.

Two existing tests were stale rather than failing, which is the worse kind:
`TestPendingIndexPartialIndexSupportsTheClaim` asserted against `000010`'s
`idx_index_outbox_pending`, which `000011` replaces with the lease-aware
`idx_index_outbox_claimable` — it would have passed forever while the index the claim actually uses
went unchecked. `TestDrainClaimsOneRowPerResourceInOrder`'s comment still claimed locks were held
"through the publish AND the retire", the precise property this change removes.

**Update** — Renumbered the outbox migration `000008_index_outbox` → `000010_index_outbox`
(LFXV2-2814, PR #60). PR #59 (stuck-claim recovery) also defines a `000008`, plus a `000009` that
repairs an INVALID index its own `000008` may leave behind, so that pair must stay together;
`main` is at `000007`. The loud failure was a merge conflict, but the quiet one is why this moved
ahead of merge rather than at merge time: two files sharing a version means golang-migrate applies
one and SILENTLY SKIPS the other, so `index_outbox` would never be created in any environment,
every brief and campaign write would fail its co-commit, and the whole indexing recovery path
would be dead with nothing in the logs pointing at the cause. Renumbering removes the ordering
dependency outright — #59 and #60 can now merge in either order, instead of correctness resting on
a merge sequence the repository does not enforce. `migrations.go` embeds `*.sql` by wildcard, so
only two hard-coded references needed updating (the test that reads the migration by filename, and
the indexer concept doc). Verified on a live PostgreSQL 16 by injecting #59's `000008`/`000009`
alongside this `000010` and running the service's own `postgres.Migrate`: all three apply and the
schema lands clean at `version=10, dirty=f`, confirming the number gap does not block the runner.

## 2026-08-03

**Update** — An unusable `NATS_URL` now fails boot (LFXV2-2814, PR #60). The publisher is built
with `RetryOnFailedConnect`, so an ordinary broker outage never reaches that error branch — it
returns a reconnecting publisher. Reaching it means the config can never work (malformed URL), and
degrading to a Noop was the worst available outcome: `NATS_URL` is non-empty, so the enqueue gate
stays OPEN and every write co-commits a row into a table this process can never drain, whose
pending rows are deliberately never pruned. The service would look healthy while accumulating
undeliverable work forever. Now fatal, matching how invalid database settings are already treated.

**Update** — Fixed a data-loss bug I introduced with the disabled-indexing gate, plus the backoff
clock (LFXV2-2814, PR #60). (1) The gate read `IndexerIsNoop()`, but `NewNATSPublisher` returns a
Noop for an UNREACHABLE broker as well as an empty `NATS_URL` — and the publisher is never
re-dialled. So a pod that started during a broker restart skipped the outbox for its entire life,
permanently losing every brief archive and campaign create (pending rows are never pruned; there
is no reindex path). Strictly worse than the growth the gate was added to prevent. It now keys on
an explicit config flag (`DisableIndexing`, set by the container only when `NATS_URL` is empty).
`TestBrokerDown_StillEnqueues` pins it — with the old gate it writes 0 rows instead of 2. (2) The
retry backoff used `now()` on both sides, which is TRANSACTION-START time; the drain holds one
transaction across the whole pass, so every backoff was understated by up to the pass duration.
Both sides now use `clock_timestamp()`. Verified on live PostgreSQL: inside a transaction, after a
2s sleep, `now()` had not advanced at all while `clock_timestamp()` advanced by 2s. Also removed
two stale comments — one asserting the OLD `Noop.PublishRaw`-reports-success contract (the exact
bug this PR fixed), one describing the reverted two-window prune.

**Update** — Added retry backoff to the outbox claim (LFXV2-2814, PR #60). `attempts` was
recorded but never affected ELIGIBILITY, so a row that can never be delivered was re-selected on
every pass — and once enough poison rows accumulate as the oldest resource heads they consume the
whole batch, so a failure stops "blocking only its own resource" and starves every newer write.
A failed row now waits `2^attempts` seconds (`last_attempt_at`, new column), capped at hourly so a
long outage still recovers. The exponent is capped separately from the seconds: `attempts` is
unbounded and `POWER(2, n)` overflows int first (verified with attempts=999999). Reproduced on
live PostgreSQL 16 with 50 poison heads + 1 new write: without backoff the batch was 50 poison
rows with the new write excluded; with backoff it was the new write alone. EXPLAIN still shows an
index scan on the partial index.

**Update** — REVERTED the pending-row pruning added earlier today (LFXV2-2814, PR #60); a
reviewer was right and I was wrong. Aging out undelivered work is unrecoverable here: there is no
full-reindex path, and the cases with no later write to repair them — a terminal brief archive, a
created-then-never-edited campaign — are exactly the ones the outbox exists for. The stated
motivation did not even hold: `drain` returns BEFORE pruning when the token is absent, so the
unprovisioned case never pruned anyway. Pending rows are now never pruned at any age. Unbounded
growth is prevented at the SOURCE instead: when indexing is deliberately disabled (`NATS_URL=""`)
the payload builder is nil and no row is written. A missing credential is deliberately not
treated that way — it is a provisioning gap and the rows are real work.

**Update** — `PublishRaw` now uses NATS request/reply instead of publish-and-flush (LFXV2-2814,
PR #60). A flush only confirms the bytes reached the broker; it says nothing about acceptance.
Checked the REAL consumer rather than assuming: `lfx-v2-indexer-service` subscribes to
`lfx.index.*` via `QueueSubscribeWithReply` and its `IndexingMessageHandler.HandleWithReply`
answers `"OK"` on success and `"ERROR: ..."` on any rejection. So a rejected message was being
retired as delivered — and, after the retention work, eventually pruned. Only a literal `OK` now
retires the row. Verified against a live nats-server: accepted → nil, rejected → an error
carrying the indexer's reason, no responder → `nats: no responders available`.

**Update** — Bounded the PENDING side of the index outbox too (LFXV2-2814, PR #60). The previous
commit pruned only published rows, so with indexing disabled (`NATS_URL=""`) or unprovisioned
(the service token is an `optional` chart secret — my own choice) every brief and campaign write
still co-committed a JSONB row that nothing would ever drain: unbounded growth in a configuration
the deployment actively permits. Pending rows now age out after 30 days, versus 7 for published
history — long enough that a row is only discarded well after any realistic outage or rotation
would have been noticed, at which point a reindex beats replaying a month-old snapshot. Verified
on live PostgreSQL 16: 30d-published and 40d-pending rows deleted; 1d-published, 29d-PENDING, and
fresh rows all kept.

**Update** — Closed four more findings on the outbox (LFXV2-2814, PR #60). (1) POOL DEADLOCK: the
guarded-update paths in `ReplaceBrief`/`Approve`/`ReplaceCampaign` classified a no-row result by
calling `GetBrief`/`GetCampaign`, which acquires a SECOND pool connection while the transaction
still holds the first — with a saturated pool (`pool_max_conns=1` makes it certain) an ordinary
stale-version request blocked until its context expired instead of returning 412. The existence
check now runs inside the same transaction, which also reads the same snapshot the UPDATE did.
(2) UNBOUNDED GROWTH: published rows were retained forever with no pruning path;
`PrunePublishedIndexMessages` now trims them after 7 days, bounded per pass, running after the
drain. Pending rows are never eligible. (3) The `bearer` parameter threaded through
`Start`/`run`/`dispatchPlatform` became dead when campaign creates moved to the outbox, and its
comment still claimed the indexer needed the request JWT — removed, along with the now-unused
`deref` helper. (4) No `published_at` index: verified with EXPLAIN that the prune's
`ORDER BY id LIMIT n` is served from the primary key, so an extra index would be pure write
amplification.

**Update** — Hardened the outbox claim and finished routing campaign writes through it
(LFXV2-2814, PR #60). (1) `SKIP LOCKED` alone did NOT preserve per-resource order: it skips an
older LOCKED row for object X and hands another pod the NEWER row for the same X, publishing an
update before its create. The claim now carries a `NOT EXISTS` predecessor check — one message
per resource per pass, so a failed delivery blocks only its own resource. Verified on a live
PostgreSQL 16: with one pod holding b1's create, a concurrent pod claimed ZERO rows. (2) Ordering
moved from `created_at` to `id`: `now()` is TRANSACTION-START time, so a transaction that began
earlier but wrote later gets an earlier timestamp and sorting by it can invert committed order.
The partial index is re-keyed `(object_type, object_id, id)` to serve both. (3) `UpdateCampaign`
and `ToggleCampaignStatus` still published directly after `ReplaceCampaign`, so a replayed create
could overwrite a newer update — `ReplaceCampaign` now co-commits too, and `publishIndex` is gone
entirely: no write publishes on the request path. (4) Because a pass now claims one row per
resource, `drain` loops while making progress (bounded), so a queued create+update+delete drains
in one tick rather than 45s.

**Update** — Closed two more outbox findings on PR #60 (LFXV2-2814), both raised against the
previous commit. (1) `PendingIndexMessages` read rows with NO claim, so every replica loaded the
same batch: a slow pod could publish an earlier `updated` after a faster one had already
published the later `deleted`, resurrecting an archived brief ACROSS replicas — the same race the
outbox closes intra-process, and rolling deploys make overlapping pods routine. The read/mark/
record triple collapsed into one `DrainPendingIndexMessages(ctx, limit, deliver)` that claims
with `FOR UPDATE SKIP LOCKED` and holds the locks across the publish (passed down as a callback)
and the retire. Ordering is now `created_at, id` — total, since `created_at` can tie at now()
resolution. (2) Campaign creates still published directly with the caller's JWT captured at
`Start`; because dispatch is async on the root context, that token could be EXPIRED by publish
time and there was no row to retry, leaving a new campaign permanently unsearchable.
`UpsertCampaign` now takes a `CampaignIndexPayloadFunc` and co-commits like the brief writes.

**Update** — Routed ALL brief writes through the index outbox (LFXV2-2814, PR #60), not just the
terminal archive. With create/replace/approve publishing directly after commit and only the
archive co-committing, the two paths could not be ordered against each other: a replace could
commit, stall before its publish, and land its update AFTER the archive had been replayed and
retired — resurrecting a deleted brief in the index with no pending tombstone to repair it. The
publisher's per-object lock cannot close this (process-local, orders only calls as they arrive).
`CreateBrief`/`ReplaceBrief`/`Approve` now take an `IndexPayloadFunc` and co-commit like
`ArchiveBrief` did, giving each brief one ordered sequence carried by the table — correct across
replicas. Replace and Approve also switched to `RETURNING` so the indexed snapshot is exactly
what committed, rather than a post-write `GetBrief` that could observe a later concurrent write.

**Update** — Closed four more review findings on the index outbox (LFXV2-2814, PR #60), all
raised against the previous fix. (1) `INDEXER_SERVICE_TOKEN` was never wired in the Helm chart,
so the new empty-token guard would have idled the relay in every cluster — added as an
`optional` `secretKeyRef` (optional so a cluster missing the key still starts; the relay idles
and rows stay pending rather than blocking pod start). (2) `Close` read `c.indexRelay` BEFORE
joining the DB-init goroutine, which is what installs the relay on the 503 cold-start path: a
retry landing in that gap started a relay nothing stopped, reading the outbox through
`pool.Close`. Reordered, and the field is now mutex-guarded like `pool` — it was an
unsynchronized read/write besides. (3) `PublishRaw` returned nil when the flush budget was
exhausted, letting the relay retire a row for a delivery never confirmed on the wire; it now
returns the context error, checked FIRST because `flushBudget` consults only the deadline and a
context cancelled without one reported a full budget.

**Update** — Closed two review findings on the index relay (LFXV2-2814, PR #60). Both were
consequences of the outbox commit itself. (1) With no `INDEXER_SERVICE_TOKEN`, `stamp` wrote an
EMPTY authorization header; NATS accepted the publish so `drain` retired the row, while the
indexer drops empty-auth messages — the outbox would have silently drained itself and terminal
writes like brief archival would have lost their only recovery path. `drain` now skips the pass
and warns once, leaving rows pending. Same defect class as the `Noop.PublishRaw` fix. (2)
`relayStopTimeout` was spent at the start of `Close` but omitted from `ContainerCloseTimeout`,
so shutdown could overrun `DefaultShutdownTimeout` and reintroduce the SIGKILL-mid-drain risk
the budget exists to prevent; it is now in the sum and the `init()` assertion still holds.

**Update** — Rebuilt the indexing contract against the REAL indexer (LFXV2-2814, PR #60 review).
**A reviewer was right and I was wrong, three times.** The flat body this PR published would
have been REJECTED before indexing — the service would have looked fully wired and indexed
nothing.

Root cause of my error: I treated `lfx-v2-query-service`'s `TransactionBodyStub` as the producer
contract. It is the `_source` shape the indexer PRODUCES after processing a message. The actual
consumer is `lfx-v2-indexer-service` (a separate repo, not checked out locally), whose
`LFXTransaction` requires `action` + `headers` + `data` + `indexing_config`, and rejects a
message with no action outright.

I searched only local checkouts, stated the caveat "the subscriber isn't in any local checkout",
and then failed to do the obvious next thing — search GitHub, where the repo exists. **Absence of
evidence in the repos you happen to have is not evidence of absence.**

The contract now mirrors the indexer: FGA metadata moved under `indexing_config`, `object_type`
removed from the payload (the indexer derives it from the SUBJECT), and create/update/delete
threaded per operation — archiving publishes `delete`, since republishing it as an update would
leave the document findable.

**Update** — Two more indexing fixes (LFXV2-2814, PR #60 review):

**Indexed documents now use snake_case.** The generated goa types (`briefs.Brief`,
`briefs.Campaign`) carry NO json tags, so publishing them directly emitted Go field names —
`"ProjectID"`, `"EventSlug"` — instead of the API's `project_id`/`event_slug`. Verified against
the real payload before fixing. Such a document indexes CLEANLY and then matches nothing for any
consumer filtering on API field names, which looks exactly like indexing being broken.
`indexer.BriefDoc` / `indexer.CampaignDoc` now restate the indexed shape explicitly with tags —
hand-written on purpose, since the projection is a contract with the Query Service and should
change only when someone edits it deliberately.

**The dial error no longer leaks the broker password.** Redacting our own `%s` prefix was not
enough: `%w` renders nats.go's error, and its URL-parse failures embed the ORIGINAL string, so a
malformed credential-bearing `NATS_URL` printed `nats://***@host` and the raw `user:pass@host`
in the SAME line (reproduced before fixing). `scrubURL` now removes the credential from the
wrapped text as well.

**Update** — Redacted the broker URL in `Config.String` (LFXV2-2814, PR #60 review). `String()`
promises a log-safe representation and redacts `DatabaseURL`/`CredentialEncryptionKey`, but
printed `NATSUrl` VERBATIM. A NATS URL may carry userinfo (`nats://user:pass@host:4222`), and
this PR is what made that field live — so anything logging the config would have put the broker
password in the pod logs.

`redactNATSURL` strips the credential but KEEPS the host, unlike `redactDatabaseURL` which masks
wholesale: the broker host is what makes an indexing outage diagnosable, and a NATS URL is always
a parseable URL (no keyword-DSN form), so the credential portion can be removed precisely.

**Update** — Toggle publish now uses the DETACHED context (LFXV2-2814, PR #60 review).
`ToggleCampaignStatus` writes on `persistCtx` (`context.WithoutCancel`) on purpose — the platform
status has already changed, so a cancelled request must still record it — but the index publish
still used the request `ctx`. That dropped the index update for exactly the cancelled requests
the detach exists to protect, leaving the database correct and search stale on the cases most
likely to need reconciling. Same class as the orchestrator's persist-site publishes.

Note the SIBLING site (`UpdateCampaign`) writes on plain `ctx` and publishes on `ctx`, which is
consistent — only the toggle mismatched.

**Update** — Closed a read-then-archive race in `DeleteBrief` (LFXV2-2814, PR #60 review). The
archive-republish fix read the brief, archived it, then published the SNAPSHOT with a
hand-incremented version. A concurrent `ReplaceBrief`/`Approve` committing in that window would
make the archive apply to the newer row while the index received the older content, at a version
that never existed in the table.

`ArchiveBrief` now RETURNS the archived row (`UPDATE ... RETURNING`), making the write and the
read of its result one statement; the port signature no longer permits a separate read. A second
archive is `ErrNotFound`, since the `status <> 'archived'` guard commits nothing.

Testing note: an in-memory fake hands back the SAME pointer for the read and the archive, so
racy and correct implementations publish identical version numbers — a version-based assertion
passes either way (verified: the first version of this test stayed green against a deliberately
racy DeleteBrief). The test therefore asserts the repository CONTRACT; the real guarantee lives
in the SQL and the port signature.

**Update** — Retracted the "self-healing" claim for indexing (LFXV2-2814, PR #60 review). Core
NATS is at-most-once, and this bundle plus `publisher.go` both asserted that a dropped message
"self-heals on the next update". That is FALSE for terminal writes: archiving a brief has no
successor write, and a created-then-never-edited campaign may never be written again — so the
index can be permanently stale or missing the only document backing lists/history.

No code fix here beyond the retraction: bounding the flush narrows the window but cannot close
the commit-to-publish gap (the process can die between the two regardless). Closing it needs
delivery recoverable independently of the write — a transactional outbox with a relay, or a
periodic database-to-index reconciliation sweep. Recorded as a KNOWN GAP rather than
implemented, because it is a design decision with operational weight and belongs in its own
change, not a review round.

**Update** — Budgeted the index drain into shutdown (LFXV2-2814, PR #60 review follow-up).
Bounding `nats.DrainTimeout` to 2s was necessary but NOT sufficient: `Container.Close` really
does spend those 2s draining after the pool closes, and `ContainerCloseTimeout` did not count
them. The two shutdown phases already consumed all 25s of `DefaultShutdownTimeout` with ZERO
headroom, so the unbudgeted drain pushed the real total to 27s — the SIGKILL-mid-drain the
budget exists to prevent.

`indexer.DrainTimeout` is now exported and folded into `ContainerCloseTimeout`, and
`dispatchDrainTimeout` trimmed 6s → 4s so the HTTP phase keeps a positive budget. The `init()`
guard now asserts the composed `ContainerCloseTimeout` rather than re-deriving two of its three
terms, so a future term added to Close cannot escape the check.

**Update** — Bounded indexing's shutdown cost (LFXV2-2814, PR #60 review). Both findings were
consequences of wiring the publisher into the shutdown path without checking its budget:

- `Close` calls `conn.Drain()`, and nats.go defaults `DrainTimeout` to **30s** — alone MORE
  than the service's entire graceful-shutdown budget (`DefaultShutdownTimeout`, 25s). A wedged
  broker would hold `Container.Close` past the budget and get the pod SIGKILLed mid-shutdown,
  defeating the very budget `ContainerCloseTimeout` exists to enforce. Now pinned to 2s.
- `Publish` flushed for a flat `publishTimeout` (3s) even during shutdown grace, which is sized
  as `persistResultTimeout + jobFinalizeTimeout + 1s` and budgets nothing for a flush between
  them. The flush now takes the smaller of `publishTimeout` and the caller's remaining
  deadline (`flushBudget`), skipping entirely when none is left — the message stays buffered
  and the drain is its last chance, which is correct for a best-effort concern.

The flush bound is asserted on `flushBudget` directly rather than by timing a real publish: an
unreachable broker fails its flush immediately, so a timing test passes even with the bound
removed. The first version of that test was vacuous for exactly that reason.

**Update** — Wired Query Service indexing (LFXV2-2814). `internal/infrastructure/indexer`
publishes brief and campaign snapshots to NATS on every write; `brief.go:169` had marked this
seam as "no indexing happens here yet" since the service was built. Architecture D1 and D5 both
commit to it, and it is why #55 needed its own lookup endpoint rather than querying the index.

The contract was DERIVED from the platform, not invented — recording the derivation so a future
change can re-verify rather than re-guess:

- Subject `lfx.index.<object_type>` matches the four already in use (`committee_document`,
  `project_document`, `individual_vote`, `vote_response`).
- The body mirrors `lfx-v2-query-service`'s `TransactionBodyStub`, whose own comment marks it as
  the indexed `_source` shape. Its searcher reads `object_type` and `public` directly out of
  `_source`, so a wrong value there indexes cleanly and then matches nothing.
- `access_check_relation = campaign_manager` on `project:<projectId>`, per architecture D2 ("no
  new FGA object types; only relations on project") and the deployed `ruleset.yaml`, which gates
  every route on exactly that. D2 also explains why the HISTORY check equals the ACCESS check:
  this service has no read-only audience.
- `public` is always false — every resource is project-scoped.

BEST-EFFORT by contract: `Publish` has no error return, an unreachable broker logs and returns a
`Noop` instead of blocking boot, and an empty `NATS_URL` disables indexing outright. The DB is
the source of truth and the Query Service re-indexes on the next write, so a dropped message
self-heals — whereas letting indexing fail a write would turn a NATS outage into a
campaign-service outage. NATS core, not JetStream, for the same reason.

No chart change was needed: `NATS_URL` was already injected via `app.environment` and already
read into `config.NATSUrl`. The only thing missing was a consumer.

**Update** — Fixed the indexing WIRING (LFXV2-2814, PR #60 review). Two independent reviewers
converged on the same defect and both were right: `SetIndexer` was called only on the 503-mode
path, so a HEALTHY startup — the normal case — kept the `Noop` and published nothing. Worse,
`SetIndexer` turned out to have zero call sites at all, because `wireLiveBackends` constructs its
own `BriefService`. The publisher is now built once in `NewContainer` ahead of every branch and
injected through `newBriefService` / `newOrchestrator`, the single funnels all five construction
sites now use.

`Orchestrator` needed the publisher too: campaign CREATES are persisted by `dispatchOne`, not by
`BriefService`, so a newly created campaign was unsearchable until some later update republished
it. Retained PARTIAL orphans publish as well — those are exactly the rows an operator must find in
order to reconcile them — and both publish on the detached `persistCtx` so a write completing
during shutdown grace still reaches the index.

Also made the documented disable switch real: `LoadConfig` resolved `NATS_URL` with
`envOrDefault`, which treats empty as absent, so `NATS_URL=""` silently became the in-cluster
default and could never disable anything. Now resolved with `envOrDefaultUnlessSet`
(`os.LookupEnv`), which distinguishes unset from explicitly empty.

Rejected one finding: a reviewer claimed the indexer requires an `action` / `headers` / `data` /
`indexing_config` envelope and drops flat messages. An exhaustive search of every local checkout
found ZERO occurrences of `indexing_config` in any code, test, doc or fixture — including the repo
that DEFINES `TransactionBodyStub`. The flat shape is corroborated on both the producer and
consumer sides (`SourceIncludes` projects those keys at the TOP level; an envelope would break
every projection). Caveat kept deliberately: the subscribing indexer service is not in any local
checkout, so this argues from absence — if that repo is produced, re-verify before trusting the
flat shape.

**Update** — Archiving now republishes (LFXV2-2814, PR #60 review). `DeleteBrief` soft-archives
the row and previously published nothing, so an archived brief kept its stale pre-archive
`_source` and went on matching searches indefinitely — every OTHER write path publishes. The
brief is read BEFORE the archive (`GetBrief` filters archived rows, so reading after would
return `ErrNotFound` and leave nothing to publish), then republished carrying the archived
status and the version bump. A read failure does not block the archive: the write is the
contract, indexing is best-effort.
**Update** — Every stuck claim now requires an upstream check (LFXV2-2665, PR #59 review).
`stuckClaimRemediation` gave a bare `version = 1` row the weaker "verify no dispatch is in flight
before deleting", on the theory that no upsert had happened so nothing could exist upstream. That
inference is WRONG, and wrong on exactly the paths this diagnostic exists to surface:
`dispatchOne` RETAINS the claim WITHOUT upserting on `(nil, nil)`, on an empty upstream id, and on
a non-pre-create `(nil, err)` — its own comments say none of those prove the provider did not
create a campaign. All three leave a row identical to an abandoned pre-create claim (version 1, no
platform id, no result blob).

The weaker guidance was therefore SATISFIABLE (the worker is gone) on a row whose paid campaign may
be live, authorizing a duplicate create — the precise failure the claim exists to prevent. Both
branches now require upstream verification and differ only in stating WHY it is owed. Guarded by
`TestStuckClaimRemediation_AlwaysRequiresUpstreamCheck`, written as an invariant over every row
shape rather than per-case strings; verified by reverting the bare-row branch, which fails on
`version=1, id="", result=0 bytes`. Runbook updated to say the same.

Credit where due: this was a Copilot suppressed comment that recurred across several rounds. It was
correct, and reading it on merit rather than dismissing it as bot noise is what surfaced it.

**Update** — Pinned the stuck-claim truncation contract with tests (LFXV2-2665, PR #59 review).
`scanStuckDispatchClaims` clamps the reported batch to `DefaultStuckClaimLimit` and sets
`truncated` when the repo's `limit+1` probe comes back saturated — the mechanism that lets an
operator tell "exactly 100 stuck" from "at least 100, real total unknown". Nothing tested it.

Added `TestScanStuckDispatchClaims_TruncationIsHonest` (under the cap / exactly the cap /
saturated) and `TestScanStuckDispatchClaims_SilentWhenClean`, asserting against the actual emitted
slog records. Each was verified by reverting the behavior it covers: dropping the clamp leaks the
raw `101` probe row into the operator-facing `count` and reports `truncated=false`; forcing
`truncated=false` alone still fails; removing the empty-result early return breaks the silent-when-
clean guarantee (a clean scan is the normal state on every replica every 5 minutes, so it must log
nothing).

One trap worth recording: the detail-log assertion was first written as
`details <= maxStuckClaimDetailLogs`, which compares the output against the very constant that
produced it and therefore holds for ANY value of that constant — raising the cap to 1000 still
passed. It is now an absolute bound (`<= 10`), which does fail on that change. A cap-vs-itself
assertion is not a test.

**Update** — Corrected the stated reason for `make_interval` in the stuck-claim scan (LFXV2-2665,
PR #59 review). The comment in `campaign_repo.go` and `stuck_claims_test.go` claimed Postgres
REJECTS the `"4m0s"` Go renders for `4 * time.Minute`, and that an earlier `$1::interval` version
"would have errored on every scan". That is false — verified on PostgreSQL 16.10, `'4m0s'::interval`
parses to `00:04:00` both as a literal and as a bound parameter.

`make_interval(secs => $1)` is still correct and unchanged; only the justification was wrong, which
matters because a future reader could "simplify" back to `$1::interval` after finding the stated
reason doesn't reproduce. The real argument is narrower: it removes a standing dependency on Go's
duration formatting matching Postgres's interval grammar. They DO diverge, just not at this value —
Go renders `100ns` and `1µs` (Unicode mu) for smaller durations and Postgres rejects both outright,
and `1.000000001s` silently truncates to `1s`. So retuning the constant to a sub-microsecond value
would break the scan at runtime rather than at compile time. Binding numeric seconds sidesteps the
grammar entirely and matches `JobRepo.FailStuckJobs`.

Also confirmed while verifying: the `000008` partial index is actually chosen by the real query.
`EXPLAIN ANALYZE` over 200k rows gives `Index Scan using idx_campaigns_stuck_claims`, 102 buffers,
and no sort node — the index supplies `created_at ASC`, so the `LIMIT` stops early as intended.
**Update** — Pinned the find-brief "no MaxLength" guarantee at the DECODER, and recorded where
`MinLength(1)` actually bites (LFXV2-2812, PR #55 review).

`TestFindBrief_HandlesLongSlugs` called `BriefService.FindBrief` directly, so it could not catch a
length cap reintroduced in `design/brief.go`: goa generates that check into
`DecodeFindBriefRequest`, which the service-level call bypasses. The test's comment nevertheless
claimed it guarded against exactly that. Added
`TestFindBriefDecoder_RejectsEmptySlugButNotLongOnes`, which routes a real request through the
goa muxer and decodes it. Verified binding by adding `MaxLength(64)` to the design and
regenerating: the new test fails, the old one still passes.

Also established, by removing it and regenerating, that `MinLength(1)` on the find-brief
`event_slug` QUERY PARAM is redundant: because the param is `Required()`, goa already rejects `""`
with a `MissingFieldError`, so no test can distinguish its presence. It is kept as
belt-and-braces and is now documented as such. The constraint that genuinely does work is the one
on `BriefWriteInput.event_slug` — a JSON BODY field, where `Required()` only checks key presence —
and `TestBriefInput_RejectsEmptyEventSlug` was confirmed binding the same way (drop the design
constraint, regenerate, test fails).

**Fix** — Corrected a stale comment on `BriefInput` in `design/brief.go` that pointed maintainers
at a `nonEmptyEventSlug()` helper which does not exist anywhere in the repo. The real mechanism is
the separate `BriefWriteInput` type; the comment now names it.
**Update** — Closed two test gaps on the X/Twitter status toggle (LFXV2-2808, PR #53 review).
Both were unguarded contracts, not bugs: the code was already correct, but nothing would have
caught a regression.

The `twitter.IsOutcomeUnconfirmed` branch in `TwitterDispatcher.ToggleStatus` was untested. The
two ambiguous shapes reach it differently, and only one depends on it to acquire the marker at
all: a first-call 5xx surfaces as a plain `apiError`, which does NOT implement `Unconfirmed()`
— `IsOutcomeUnconfirmed` recognizes it structurally via `createOutcomeAmbiguous`, and the
dispatcher's wrap is what turns that into an error carrying the behavioral marker. A partial
cascade already implements `Unconfirmed()` itself, so it would survive the branch's removal.
Deleting the wrap therefore left every client test green while the 5xx outcome silently
degraded to "not modified". Twitter was the only toggle-capable dispatcher without this
boundary test (googleads, reddit and meta all had one). Pinned in BOTH directions — a definite
4xx on the first call mutated nothing and must stay definite — so an unconditional wrap fails
too.

The deliberate create/toggle credential asymmetry was also undefended: `Dispatch` requires
`funding_instrument_id`, `validateTwitterConnection` intentionally does not, because
`UpdateCampaignAndChildrenStatus` only PUTs `entity_status` on entities that already exist and
never puts that field on the wire. Requiring it in the shared validator would refuse an
otherwise-valid pause. A future refactor folding the check into the shared validator now breaks
a test instead of silently restoring the rejection the asymmetry exists to avoid. Recorded in
`internal-dispatch.md` so the constraint is discoverable before someone attempts that tidy-up.

**Fix** — A mutating 429 whose retry backoff is interrupted by the caller's deadline was
misclassified as DEFINITE. `doRequest` returned the bare `ctx.Err()` from `sleepCtx`, erasing
the 429 the server had already sent — and a mutating 429 is ambiguous, since X can report the
throttle at or after the write is accepted. Reachable in production rather than theoretical:
`maxRetryWait` (90s) exceeds the orchestrator's `toggleCallTimeout` (45s), so a server-declared
`Retry-After` in between (e.g. 60s) passes the cap check, is accepted for sleeping, and is then
cut short by the toggle deadline. The operator was told "not modified" about a pause that may
well have applied — exactly the failure mode `partialCascadeError` exists to prevent, arriving
through a different door.

The client now returns the 429 as a typed `apiError` with the cancellation cause attached via a
new `Unwrap`, so `IsOutcomeUnconfirmed` still classifies it while `errors.Is(err,
context.DeadlineExceeded)` keeps working for callers that distinguish deadlines. `Error()` is
unchanged and still renders only method/path/status, so nothing body-derived reaches a persisted
Step. Also corrected `docs/architecture.md`, which still listed the X/Twitter and Google Ads
toggles as follow-up work after both had landed; Microsoft is now the only outstanding one.

## 2026-08-03

**Update** — Clear an INVALID stuck-claim index (LFXV2-2665, PR #59 review, migration 000009).
A failed `CREATE INDEX CONCURRENTLY` does NOT roll back — it leaves the index marked INVALID.
`IF NOT EXISTS` then sees that name, skips the rebuild and reports success, so the scan keeps
full-scanning forever with no error anywhere. `force`-recovering a dirty migration marks the
version applied WITHOUT running the down migration, so nothing else clears it.

000009 drops an INVALID copy AND rebuilds it in the same step (a plain DROP+CREATE inside a DO
block — an invalid index serves no query, so nothing that was working is blocked, and neither
form of CONCURRENTLY can run inside the conditional). It must do both: `force`-recovering the
dirty schema marks version 8 applied WITHOUT running it, so golang-migrate never re-executes
000008 and its `IF NOT EXISTS` would skip regardless. Recovery therefore requires operator
force + a subsequent deploy to apply 000009 (which then drops the INVALID and rebuilds VALID).
This is either automatic (next deploy) or manual (reissue `CREATE INDEX CONCURRENTLY
idx_campaigns_stuck_claims ON campaigns (created_at) WHERE status = 'pending'`) — waiting for
deployment alone is the correct path. A VALID index is untouched, and both object names are
schema-qualified so a future multi-schema setup cannot inspect one index and drop another.
Verified on live PostgreSQL 16 across all three paths: INVALID → dropped and rebuilt VALID
with a definition identical to 000008; healthy → no-op; absent → no-op, no error.

**Update** — `idx_campaigns_stuck_claims` is now built CONCURRENTLY (LFXV2-2665, PR #59 review).
A plain `CREATE INDEX` takes a lock blocking INSERT/UPDATE/DELETE on `campaigns` for the whole
build, and migrations run during a ROLLING startup — other replicas are still claiming and
finalizing dispatches at that moment, so a blocking build could stall a claim mid-flight and
MANUFACTURE the ambiguous outcomes this diagnostic exists to report.

Safe with this runner, and verified rather than assumed: the pgx/v5 golang-migrate driver
executes each migration with a bare `ExecContext` and does NOT wrap it in a transaction
(`database/pgx/v5/pgx.go`), which `CONCURRENTLY` requires. Keep this migration SINGLE-statement
— a multi-statement file would be batched and reintroduce the transaction constraint. The down
migration drops concurrently too, and clears the INVALID index a failed concurrent build leaves
behind so a retry is clean.

Also renamed `TestStaleClaimAgeExceedsProviderCallTimeout` →
`TestStuckClaimReportAgeExceedsProviderCallTimeout` (@dealako): it asserts on
`stuckClaimReportAge`, so the old name referenced a constant that never existed.

**Update** — Added `idx_campaigns_stuck_claims` (LFXV2-2665, PR #59 review, migration 000008).
The stuck-claim scan filters `campaigns` on `status = 'pending' AND created_at < …` and now runs
every 5m on EVERY replica, with no supporting index — a full scan that grows unbounded as
terminal campaign rows accumulate, while the set it cares about ('pending' claims) stays tiny and
is usually empty.

A PARTIAL index on `created_at WHERE status = 'pending'` keeps the index small and also serves
the query's `ORDER BY created_at ASC`, so the `LIMIT` stops early instead of sorting the whole
match set. This mirrors `idx_campaign_jobs_recovery` (000004), which exists for the same reason
on the analogous stuck-JOB sweep — the periodic sweep added in this PR is what made the index
necessary rather than merely nice.

**Update** — Corrected the sweeper-stop reasoning (LFXV2-2665, PR #59 review). The bounded wait
was right but its justification was WRONG: the comment claimed an abandoned sweeper "holds no
pool reference". It does — `StuckDispatchClaims` runs a `pgxpool.Query`, which holds a pooled
CONNECTION until its rows close, and pgxpool's `Close` is documented as blocking "until all
connections are returned to pool and closed". So giving up on `<-sweepDone` does not bound
shutdown on its own; `pool.Close()` would block on the same scan, just later and less visibly.

What actually releases the connection is CANCELLING the sweeper context — pgx aborts the
in-flight statement on cancellation and returns the connection. The wait exists only to let
that release complete in the common case. The new test asserts the ordering invariant (the
scan's context is cancelled by the time `Close` returns) rather than a nil-pool no-op; the
previous test passed for the wrong reason because it deliberately used a nil pool.

**Update** — Bounded the sweeper stop in `Close` (LFXV2-2665, PR #59 review). Cancelling
`sweeperCtx` interrupts a scan but does NOT guarantee it returns: a driver already inside a
statement can take until `stuckClaimScanTimeout` (5s) to unwind, and a scanner that ignores
cancellation never returns at all. That wait sat BEFORE the dispatch drain, so it spent the
drain's budget on a diagnostic — starving the phase that protects in-flight campaign creation.

`Close` now waits at most `sweeperStopTimeout` (250ms) and abandons the goroutine on timeout;
it holds no pool reference beyond its own bounded scan and only logs. The test wedges a scanner
that ignores cancellation: with the unbounded wait, `Close` deadlocks (the test times out at
30s) rather than merely running slow.

**Update** — Two follow-ups on the stuck-claim scan (LFXV2-2665, PR #59 review):

- The COLD-START scan ran on `context.Background()`, so a scan blocked in the database could
  not be interrupted and `Close`'s `<-c.initDone` wait would overrun the bounded shutdown
  budget by up to `stuckClaimScanTimeout`. It now derives from the init `ctx`, matching the
  adjacent `FailStuckJobs` call which already did this for the same reason.
- The per-row log implied a safe case that does not exist. The code comment said `version > 1`
  distinguishes an ambiguous outcome from a bare claim "usually safe to delete", but
  `upserted_after_claim=false` would read as SAFE for a claim whose dispatch is still in flight
  — those look identical in the row. An explicit `remediation` field now states what must be
  verified, and never says "safe to delete".

**Update** — Added a periodic stuck-claim sweep (LFXV2-2665, PR #59 review). The startup scan
alone left a real gap, and it is the COMMON case: a claim stranded seconds before a rolling
deploy or crash-restart is YOUNGER than `stuckClaimReportAge` (4m), so the replacement pod's
boot scan skips it — and nothing ever looked again, leaving the row silently blocking every
future dispatch for its `(brief_id, platform)`. `startStuckClaimSweeper` re-scans every
`stuckClaimSweepInterval` (5m, matching the orchestrator's `recoverySweepInterval`, which
exists for the same reason applied to jobs).

Still REPORT-ONLY — nothing reclaims or deletes. `pending` cannot distinguish a claim in
flight from an ambiguous outcome where a paid campaign may already exist upstream, so a
time-based takeover could authorize a duplicate paid create. That reasoning is unchanged.

The sweeper is stopped by `Close`, deliberately AFTER the `<-c.initDone` wait: on the
cold-start path the retry goroutine is what assigns `cancelSweep`, so reading it earlier
would be an unsynchronized read that could also miss a sweeper started moments later.

**Update** — Made stuck dispatch claims VISIBLE (LFXV2-2665, partial). A pod crashing between
`ClaimCampaignDispatch` and `releaseClaim` strands a `pending` campaigns row which, because the
claim is `ON CONFLICT (brief_id, platform)`, blocks EVERY future dispatch for the pair — with no
signal anywhere. Operators discovered it only when someone reported a campaign not dispatching.
`StuckDispatchClaims` reports `pending` rows older than `stuckClaimReportAge` (4m, above
`providerCallTimeout` so healthy in-flight work is never flagged), bounded by a row limit.

**Attempted and REVERTED before merge**: auto-reclaiming an expired claim via
`ON CONFLICT DO UPDATE`. Review (copilot) correctly showed it was unsafe — `pending` is
OVERLOADED, marking both a claim in flight AND an ambiguous dispatch outcome that the
orchestrator persists as `pending` precisely because a paid campaign MAY already exist upstream.
No column distinguishes them, so the reclaim would eventually authorize a duplicate paid create:
the exact failure the claim exists to prevent. Recording the dead end so it is not re-attempted
— safe auto-recovery needs provider idempotency keys or an authoritative reconcile first, both
still open under LFXV2-2665. The linkedin concept, which called single-flight merely "planned", now states the
reality: single-flight EXISTS (the unique-index claim), the claim is NOT reclaimed on a
timer, and a crashed holder strands it until a human acts.
**Update** — Split the brief WRITE payload from the response type (LFXV2-2812, PR #55 review).
Putting `MinLength(1)` on `BriefInput` fixed the create side but broke the read side: the `Brief`
RESPONSE type `Reference()`s `BriefInput`, and goa COPIES validations through `Reference`, so the
constraint landed in all five response validators (12 generated checks). Any already-persisted
empty-slug row then became undecodable by generated clients — breaking even `get-brief` for
exactly the rows the fix exists to prevent going forward.

Created `BriefData` (unconstrained, for responses) and kept `BriefInput` (constrained, for
create/update). Verified by counting the generated checks: **0** response-side, **2** request-side.
Redeclaring the attribute on `Brief` does NOT work — goa merges rather than overrides, so the
constraint survives; a separate type is required. This approach also preserves backward
compatibility: `BriefInput` stays the same generated type name, and tooling referencing the
OpenAPI component doesn't break.

**Update** — Closed an event_slug validation gap (LFXV2-2812, PR #55 review, @dealako). The
find-brief lookup enforces `MinLength(1)` on `event_slug`, but `BriefInput.event_slug` — the
CREATE contract — had only `Required()`. goa's `Required()` checks that the JSON key is PRESENT,
not that the string is non-empty, and the `TEXT NOT NULL` column accepts `""` too.

So a brief with an empty slug was creatable, occupied the `UNIQUE(project_id, event_slug)`
index, and could then NEVER be recalled through the lookup — the caller got a 400 rather than
the documented 404 (no brief yet) or 200 (found), and a re-create collided.

`BriefInput.event_slug` — the create/update payload — now carries `MinLength(1)` so the two
contracts agree. It could NOT go on `BriefData`: the `Brief` response type `Reference()`s that,
and goa copies validations through `Reference`, so constraining it would make an
already-persisted empty-slug row undecodable by generated clients. The comment on
the lookup asserting that an empty slug "can never match a stored row" was simply FALSE and has
been replaced with the real reason it is safe: the create side now rejects it. Loosening either
constraint reopens the gap. `TestBriefInput_RejectsEmptyEventSlug` asserts the GENERATED
validator, so dropping the constraint in the design and regenerating fails the test.
**Update** — Registered the Microsoft dispatcher (LFXV2-2804, PR #50 review). The PR added the
adapter but `registerDispatchers` had no `ProviderMicrosoftAds` entry, so a brief selecting
microsoft recorded a job that finished "failed: no dispatcher registered" — the whole feature
was unreachable in production. The exact-membership test now covers it, so dropping the wiring
fails a test rather than shipping silently.

A follow-up attempt to refine the failure classification was REVERTED: it branched on
`AlreadyExisted`, but the client sets that only on the SUCCESS path (`adgroup_ad.go:363`,
immediately before `return r, nil`), so on the error path it is always false — dead code that
read like a real distinction. Separating "definitely rejected" from "genuinely ambiguous" needs
the client to classify its own partials, which is its own change.

## 2026-08-03

**Update** — Modelled the paid-ads vs email channel distinction (LFXV2-2813). `model.ChannelKind`
(`paid-ads` / `email`) with `Provider.Kind()` and `Provider.IsPaidAds()`. Previously the split
existed only implicitly: `adPlatformProviders` was named for ad platforms but CONTAINED hubspot,
the email channel, and any code needing the distinction had to compare against `ProviderHubSpot`
directly.

Renamed that roster to `dispatchableProviders` — it gates DISPATCH, and email is dispatchable
(it stages a draft) even though it is not an ad platform. `logMissingDispatchers` now logs each
missing provider's channel kind, so a missing paid platform (budget unspent) is
distinguishable from a missing email channel (no drafts staged).

The `ErrToggleUnsupported` 400 now explains WHY: for email there is nothing to pause by design,
versus an ad platform whose toggle is not wired yet. A single generic message read as a missing
feature and invited someone to "fix" the email case.

`Kind()` enumerates providers explicitly rather than defaulting, so an unclassified new provider
returns `""` and is caught by `TestProviderValidityHoldsForEveryProvider` instead of silently
inheriting paid-ads behaviour. Also made `TestLogMissingDispatchers_SurfacesGaps` rot-proof: it
now removes one provider from the real map (a synthetic gap) rather than asserting a specific
provider is still unregistered, which broke each time an adapter landed.

## 2026-08-02

**Update** — Bounded the Claude fallback's rerun in
[Local pre-PR review](architecture/local-pre-pr-review.md) (PR #56 review,
LFXV2-2905). On a role-level host failure the trio is rerun **once**; if the rerun
also fails, the role-labelled failure is reported and the launcher stops. The
reviewer-authored `INCOMPLETE — <reason>` result and a host-side fallback failure
remain separate states — the bound does not merge them.

## 2026-07-31

**Fix** — Corrected two overstatements in
[Local pre-PR review](architecture/local-pre-pr-review.md) (PR #56 review,
LFXV2-2905). "Nothing consults a remote, so the cycle works offline" was broader
than the reviewer skills actually say: they permit optional read-only GitHub
inspection to inform judgement. The invariant is narrower and is now stated as such
— nothing fetches or consults a remote to *derive* the reviewed range. Separately,
"reviews exactly one commit" is the default, not an invariant; the same concept's
base-pinning section already documented the caller-supplied wider base.

**Update** — Added [Local pre-PR review](architecture/local-pre-pr-review.md)
(PR #56, LFXV2-2905): the repo-owned local review cycle. Two physical reviewer
brains under `.claude/skills/` with generic symlink aliases, an empirical knowledge
base at `docs/reviews/knowledge-base/`, and a `local-review-fallback` launch table
for three Opus subagents when Pi is unavailable. Reviews exactly
`git diff <base_sha> <target_sha>` with the target's first parent as the default
base — no fetch, no remote, no merge-base. The false-positive floor is read at both
revisions and suppresses only when both agree, so a change cannot waive a finding
about itself. Ordinary patterns remain target-only; that gap is documented in the
concept as a deferred, unsolved follow-up rather than presented as handled.
`local-agents/` is now git-ignored.

## 2026-07-30

**Update** — Added `find-brief` (LFXV2-2812): `GET /projects/{project_id}/briefs?event_slug=`
returns the saved brief for an event, or 404 when none exists. This closes the
generate-once/recall-later loop for the Campaigns Planning tab: the AI brief generation lives
in the UI's Express BFF (`POST /brief/generate`), and this service persists the result — but
nothing mapped an event URL back to the stored brief, because `get-brief` needs a brief id the
caller does not have when pasting a URL.

A 404 is an ORDINARY outcome, not a failure: first-time generation is the common case, and the
caller generates then POSTs to `create-brief`. The endpoint never generates or mutates —
regeneration stays an explicit `update-brief`, so a marketer's edits to the AI copy are never
silently clobbered (the existing `version`/`If-Match` gate protects them).

No migration: the lookup reuses `uq_campaign_briefs_project_event`, the partial unique index on
`(project_id, event_slug) WHERE status <> 'archived'`. Archiving therefore frees the slug and a
re-paste correctly 404s into a fresh generation.

On D5 (Query Service owns lists): this is a KEYED item read, not a list — the unique index
means it matches at most one brief, returning the same one-item-plus-ETag shape as
`GET /briefs/{id}`, which D5 retains. Recorded in api-catalog.md next to the rule.
**Update** — X/Twitter campaign status toggle (LFXV2-2808). `TwitterDispatcher` now implements
`service.StatusToggler`, closing the toggle gap for the twitter adapter. New client method
`UpdateCampaignAndChildrenStatus` PUTs `entity_status` (query params, not a JSON body — the same
X Ads v12 contract `createRequest` documents) and a new exported `IsOutcomeUnconfirmed` mirrors
the reddit client so the dispatcher can classify ambiguous outcomes across the package boundary.

SCOPE is deliberately campaign + line item, NOT the promoted tweet: the create path leaves the
association ACTIVE (the endpoint does not accept `entity_status`) and the LINE ITEM is X's
delivery gate, so the association never needs to move. ORDER mirrors reddit — child first on
ACTIVATE (nothing serves until the tree is ready), campaign gate first on PAUSE (delivery stops
immediately). An ACTIVATE with an unknown line-item id is refused up front as
`ErrCampaignNotProvisioned` → 409, never calling X.

Connection rules are shared via `validateTwitterConnection`, which BOTH `Dispatch` and
`ToggleStatus` call, so a create and a toggle accept exactly the same connections and cannot
drift. Each caller keeps its own error wrapping (`Dispatch` wraps with `notCreated` for claim
semantics; the toggle path must not). `funding_instrument_id` is checked only in `Dispatch` —
it is a create-time field a toggle never uses, so requiring it would refuse a legitimate pause.
A post-gate cascade failure returns `partialCascadeError` (`Unconfirmed() == true`) so a
definite 4xx on the second entity is not misreported as "not modified" when the first already
changed. `twitterChildIDs` reads the persisted `CampaignResult`
blob, whose shape is pinned by a round-trip test (the blob is `json.Marshal` of an UNTAGGED
struct, so the field is `LineItemID`; a renamed/nested field would silently yield "" and turn
every ACTIVATE into a spurious 409).
**Update** — Patched four indirect-dependency CVEs flagged by Dependabot on main
(LFXV2-2811): `google.golang.org/grpc` 1.82.0→1.82.1 (HIGH, xDS RBAC + HTTP/2, reached via
`goa.design/clue/debug`), `github.com/apache/thrift` 0.22.0→0.23.0 (HIGH,
`TFramedTransport` integer overflow, via `gosnowflake`→`arrow-go`),
`aws-sdk-go-v2/service/s3` 1.53.1→1.97.3 and `aws-sdk-go-v2/aws/protocol/eventstream`
1.6.2→1.7.8 (MEDIUM, EventStream decoder panic/DoS, via `gosnowflake`).

Manifest-only (`go.mod`/`go.sum`); no source changes. All four are RUNTIME scope, so they
are patched rather than added to `.grype.yaml` — that ignore list is reserved for the
test-only `docker/docker` transitives (via migrate/dktest) and must not grow to cover
shipping code. Recorded in the Grype section of `megalinter-secrets.md`.

**Update** — Google Ads campaign status toggle (LFXV2-2809), stacked on the GA dispatcher
(PR #41). `GoogleAdsDispatcher` implements `service.StatusToggler`; new client method
`UpdateCampaignStatus` sends a `campaigns:mutate` UPDATE with `updateMask: "status"`, and a new
exported `IsOutcomeUnconfirmed` mirrors the reddit/twitter clients for cross-package
classification.

PAUSE only — ACTIVATE is REFUSED with `ErrCampaignNotProvisioned` (→409, no upstream call),
because the GA create path provisions only a campaign SHELL (budget → campaign) with no ad
group, ad, or keywords: enabling the campaign would report success while nothing can serve. No
cascade for the same reason — there are no children yet. GA-3+ must add both a cascade and a
real child-id activate guard. Google spells the
serving state ENABLED, not ACTIVE — `googleAdsRunStatus` maps the service vocabulary across.

`mutateOperation` gained `Update`/`UpdateMask` fields, and `Create` became `omitempty` so an
update no longer emits `"create":null` alongside its update (a :mutate operation must carry
exactly ONE of create/update/remove). The create path is unaffected — it always sets Create.

Connection rules are shared via `validateGoogleAdsConnection`, called by BOTH `Dispatch` and
`ToggleStatus`, so a create and a toggle cannot drift; each caller keeps its own error wrapping
(`Dispatch` wraps with `notCreated` for claim semantics, the toggle path does not). The
campaign id is validated digits-only before any request, since it interpolates into a
resourceName.

## 2026-07-29

**Update** — Unblocked MegaLinter, which had failed on `main` since ~2026-06-29 and
blocked every open PR (#41, #46, #47, #50, #51 all showed the identical failure).
Every other check passed; the sole blocking linter was `secretlint`, with five
false positives in test code: a userinfo URL in `internal/dispatch/creds_test.go`
asserting the snapshot sanitizer fails closed, two PEM blocks in
`internal/platform/snowflake/client_test.go` asserting garbage/malformed PKCS8
bodies error out (real EC keys there are generated at runtime via
`ecdsa.GenerateKey`), and two userinfo URLs in
`internal/platform/twitter/client_test.go` asserting they are stripped and
rejected. Each flagged literal is the *subject* of its assertion, so it cannot be
removed or obfuscated. Suppressed them with per-line
`// secretlint-disable-line -- <reason>` directives, matching the existing
convention in `internal/infrastructure/config/config_test.go`, and updated the
`megalinter-secrets.md` concept with a Secretlint section. A `.secretlintignore`
path exclusion was deliberately rejected: unlike the `*_test.go` allowlist in
`.gitleaks.toml`, it would silence every rule for all current and future test
files, letting a real credential in a new test bypass both scanners. Also noted
that `REPOSITORY_SECRETLINT_FILTER_REGEX_EXCLUDE` is inert here because secretlint
runs in MegaLinter's `project` CLI lint mode.
**Update** — Added the HubSpot (email channel) PlatformDispatcher (LFXV2-2777, Capability C —
staging). `registerDispatchers` now wires `model.ProviderHubSpot` →
`dispatch.NewHubSpotDispatcher`, which — unlike the ad adapters — STAGES a marketing email
rather than creating a campaign: it resolves the HubSpot connection, fetches the brief's BUILT
audience from `campaign_audiences` (via a new narrow `audienceReader`, refusing when the newest
hubspot audience is not `built`), CLONES the caller's template (`hubspotConfig.sourceEmailId`) as
a DRAFT, and sets the clone's send list to the audience's `PlatformMasterListID` +
`SuppressionListIDs`. The cloned email id is the campaign's `PlatformCampaignID`. Claim contract:
UNCONFIRMED clone → name-only partial (claim retained); post-clone send-list failure → partial
(email exists); definite pre-clone failure → claim released. `registerDispatchers` gained an
`*postgres.AudienceRepo` arg (both container call sites updated); the container test's
registered-provider set now includes hubspot. AI body content (LFXV2-2775) and audience building
(LFXV2-2774) remain separate stories.

## 2026-07-28

**Update** — Added the Microsoft Advertising (Bing) PlatformDispatcher (MS-3, LFXV2-2805,
PR for feat/LFXV2-2805-microsoft-dispatcher). `registerDispatchers` now wires
`model.ProviderMicrosoftAds` → `dispatch.NewMicrosoftDispatcher`, so Microsoft campaigns
dispatch upstream instead of recording "no dispatcher registered". The adapter resolves the
OAuth2-app + developer-token + refresh-token connection, maps the brief + `microsoftConfig`
(`budget` in ACCOUNT currency as the DAILY budget, optional `timeZone`) onto the client's
`CreateCampaign`, which builds the full Campaign → AdGroup → Ad hierarchy (all PAUSED), and
maps the result back to a `model.Campaign` (budget/type/config persisted via
`applyCampaignConfig`, parity with the siblings). AccountID → `CustomerAccountId` (digits-only,
trimmed); `customer_id` → optional `CustomerId`. `NameSuffix = brief.ID` for retry-safe
idempotency. Non-nil result + error = UNCONFIRMED partial (claim retained); (nil, err) =
nothing created (claim released). Removed Microsoft from the `logMissingDispatchers` gap list;
updated the container test's registered-provider set to include it.

## 2026-07-24

**Update** — Review-hardened the Microsoft campaign contract (PR #44, copilot):
the clean-abort classification in `CreateCampaign` now gates on `ctx.Err()` (the
CALLER's context), not `errors.Is(err, context.DeadlineExceeded)` — the client
wraps each attempt in its own `context.WithTimeout`, so a per-attempt timeout with
a live caller context is a FAILED lookup (UNCONFIRMED), not a clean abort. Also, a
duplicate-name self-heal whose reconciliation re-lookup errors now surfaces that
cause. Aligned the `internal-platform-microsoft` concept + the older log entry to
the corrected `ctx.Err()` distinction and the duplicate-name-REJECTED contract.

## 2026-07-23

**Update** — Microsoft Ads MS-2.5 PR #45 review follow-up (copilot + cursor). (1) The ≥1-word
RSA asset rule now also covers AUTO-composed copy: `boundedUniqueCopy` drops any wordless
candidate (shared `hasWord` helper), so a punctuation-only `EventName` — which survives
`sanitizeNamePart` non-empty — can no longer become a headline that AddAds rejects after the
PAUSED campaign/ad group exist. (2) Added an up-front DISPLAY-DOMAIN check: the composed
`FinalUrls` host is validated against Microsoft's 67-char display-URL limit (RSA sets no
`Path1`/`Path2`, so the host is the whole budget); an over-long host passed the 2,048-char
check but was rejected only at AddAds. (3) Test fix: the userinfo `RejectsBadAdURL` fixture is
built at runtime via `url.UserPassword` instead of a `user:pass@host` literal that tripped
secretlint (mirrors the reddit client tests).

**Update** — Microsoft Ads MS-2.5 v13-contract hardening (PR #45 review, copilot + cursor —
VERIFIED against learn.microsoft.com). (1) RSA copy limits are WIDTH-AWARE: normal copy
30/90; Microsoft documents a reduced 15/45 cap "for languages with double-width characters"
(CJK/Korean/Japanese/Chinese/emoji). v13 gives no per-character weighted formula, so the
client conservatively applies 15/45 whenever ANY double-width char is present (never
over-length, may truncate mixed copy slightly short) — validation AND truncation both do
this. (2) Each asset must contain ≥1 word
and no newline (enforced up front). (3) The composed `FinalUrls` (registration URL + utm_*) is
length-checked against Microsoft's 2,048-char limit up front (not just the raw URL). (4)
`AddAdGroups` body carries the docs-required `ReturnInheritedBidStrategyTypes` (reserved; sent
`false`). (5) Context handling: a BARE context.Canceled/DeadlineExceeded from a read lookup is
a clean abort (nothing created), while a ctx-cancel wrapped in transportError (mid-flight
create) stays UNCONFIRMED — classification reordered so createOutcomeAmbiguous wins first; and
a ctx check runs before the ad step so a done context doesn't fire ad HTTP work. (6) Doc fixes:
`CampaignResult.AdID`/`AlreadyExisted` and `CampaignInput.Headlines/Descriptions` comments
corrected (RSA, all-three-level AlreadyExisted, width-aware limits).
**Update** — Registered the Google Ads PlatformDispatcher (LFXV2-2643, PR #41).
**Update** — Registered the Google Ads PlatformDispatcher (LFXV2-2636, PR #41).
`registerDispatchers` now wires `model.ProviderGoogleAds` →
`dispatch.NewGoogleAdsDispatcher`, so Google Ads campaigns dispatch upstream instead of
recording "no dispatcher registered". The adapter resolves the OAuth2-app + developer-token
connection (clientId/secret/refreshToken/developerToken, plus AccountID = customer id and
an optional `login_customer_id` MCC from ProviderConfig), maps the brief + `googleAdsConfig`
(`budget` in ACCOUNT currency, no FX) onto the client's `CreateCampaign`, and maps the
result back to a `model.Campaign` (persisting budget/type/config via `applyCampaignConfig`).
Uses `NameSuffix = brief.ID` for deterministic, at-most-once-retry budget/campaign names.
Release decision keys on `result == nil` alone so an ambiguous/duplicate-name create (a
non-nil name-only result) retains the claim; the possibly-orphaned budget is reconcilable by
`CampaignBudgetName` PRE-attachment, but by `CampaignBudgetID` once the campaign attaches (a
non-shared budget's name then synchronizes to the campaign name) — the partial carries both.

## 2026-07-23 (6)


**Update** — LinkedIn status toggle now CASCADES to creatives (LFXV2-2807, PR #47).
CreateCampaign leaves the campaign PAUSED and its creatives DRAFT, so activating only the
campaign would not serve (a DRAFT creative never serves; a creative's effective status is
gated by its campaign). `linkedin.UpdateCampaignAndCreativesStatus` PARTIAL_UPDATEs the
campaign status, DISCOVERS the creatives via the creatives FINDER
(`GET /adAccounts/{acct}/creatives?q=criteria&campaigns=List(urn:li:sponsoredCampaign:{id})`,
X-RestLi-Method: FINDER — LinkedIn persists only a creative count, not ids), and
PARTIAL_UPDATEs each creative's `intendedStatus`. On a PAUSE a definite 400 on an in-review
creative is tolerated (LinkedIn forbids pausing an in-review creative). Verified the finder +
intendedStatus contracts on learn.microsoft.com.

## 2026-07-23 (5)


**Update** — Meta status toggle now CASCADES like Reddit (LFXV2-2807, PR #47 review). Meta's
CreateCampaign PAUSES the campaign, ad set, AND ads, so toggling only the campaign to ACTIVE
would not serve. Added `meta.UpdateCampaignAndChildrenStatus`: POST status to the campaign,
the persisted ad set id, and each ad DISCOVERED via `GET /{adSetID}/ads` (Meta stores the ad
set id in CampaignResult but not the individual ad ids). Activate-without-ad-set-id is refused
before any call; a child failure after the campaign POST is a `partialCascadeError`
(Unconfirmed → 503-verify). `MetaDispatcher.ToggleStatus` reads the ad set id from the persisted
`*model.Campaign`. (LinkedIn was single-node at this point; a later entry above adds its
creative cascade, so all three platforms now cascade.)

## 2026-07-23 (4)

**Update** — Reddit status toggle now CASCADES to child entities (LFXV2-2806, PR #46 review).
CreateCampaign PAUSES the campaign, ad group, AND ad, so the original toggle (campaign only)
would activate a campaign whose children stayed PAUSED — it would not serve. Added
`reddit.UpdateCampaignAndChildrenStatus` (status-dependent fail-closed ordering: ACTIVATE
lifts children first then the campaign gate last, PAUSE flips the campaign gate first then the
children; skipping empty child ids) alongside the retained single-entity `UpdateCampaignStatus`. The
`StatusToggler.ToggleStatus` interface now takes the full persisted `*model.Campaign` (not just
the platform id) so the reddit adapter reads the child ids from the stored `CampaignResult`
(`adGroupId`/`adId`); single-node platforms (Meta/LinkedIn) ignore the extra context.

## 2026-07-23 (3)


**Update** — Campaign status toggle extended to LinkedIn (LFXV2-2807, on PR #47 with Meta).
`linkedin.UpdateCampaignStatus` uses LinkedIn's RestLi PARTIAL_UPDATE (POST
/adAccounts/{acct}/adCampaigns/{id}, header X-Restli-Method: PARTIAL_UPDATE, body
{"patch":{"$set":{"status": ACTIVE|PAUSED}}}) — VERIFIED against Microsoft Learn LinkedIn
Marketing API docs. `doRequest` gained an optional per-call headers map to carry the
X-Restli-Method header (5 existing call sites updated to pass nil). `linkedin.IsOutcomeUnconfirmed`
+ `LinkedInDispatcher.ToggleStatus`. Reddit, Meta, and LinkedIn now implement StatusToggler;
X/Twitter + GoogleAds follow once their dispatchers land on main (#39/#41). Tests are race-safe
(channel capture, per the #47 review).

## 2026-07-23 (2)


**Update** — Campaign status toggle extended to Meta (LFXV2-2807, follow-up to the Reddit
toggle #46). `meta.UpdateCampaignStatus` (POST /{campaignID} {"status": ACTIVE|PAUSED} — Meta
updates a node by POSTing to its id) + `meta.IsOutcomeUnconfirmed` (exposes the shared
ambiguity classifier) + `MetaDispatcher.ToggleStatus` (resolves creds, wraps an UNCONFIRMED
outcome in unconfirmedToggleError). Reddit + Meta now implement StatusToggler. X/Twitter's
toggle is deferred until the TwitterDispatcher lands on main (it's in the unmerged #39) —
tracked in LFXV2-2807.

## 2026-07-23

**Update** — Campaign status toggle (LFXV2-2806, PR #46). New
`PATCH /projects/{p}/briefs/{b}/campaigns/{id}/status` {active|paused} that pauses/resumes a
campaign ON THE AD PLATFORM then persists (previously `update-campaign` only wrote the DB row,
so a "paused" status didn't actually pause the campaign). Reddit first. Adds
`reddit.UpdateCampaignStatus` (PATCH `configured_status`), an optional `StatusToggler`
dispatcher interface + `RedditDispatcher.ToggleStatus` (via a shared `resolveRedditClient`),
`Orchestrator.ToggleCampaignStatus` (type-asserts the toggler), and
`BriefService.ToggleCampaignStatus` (platform-first, DB-after-confirm on a WithoutCancel
context, If-Match guarded, classified errors: 409 not-provisioned / 400 unsupported / 503
platform-failure). `model.CampaignRunActive/Paused` constants. Meta + X/Twitter toggles are a
follow-up. Review-hardened per dealako-sim + cursor + copilot (error classification, cancel
safety, `?`/`#` path-guard, dedup).

## 2026-07-22

**Update** — Registered the twitter (X) PlatformDispatcher (LFXV2-2642, PR #39).
`registerDispatchers` now wires `model.ProviderTwitterAds` →
`dispatch.NewTwitterDispatcher`, so twitter campaigns dispatch upstream instead of
recording "no dispatcher registered". The adapter resolves the OAuth1 4-tuple connection,
maps the brief + `twitterConfig` (opaque-JSON: `budgetAmount` in ACCOUNT currency, flight
dates, optional tweet id / destination URL) onto the client's `CreateCampaign`, and maps
the result back to a `model.Campaign` — marking `created_degraded` when the promoted tweet
is unconfirmed/absent or the campaign/line-item was reused. Client changes landing with
it: a `Reused` reuse/config-drift flag on `CampaignResult`; an exhausted mutating 429
classified UNCONFIRMED; destination-URL validation (https/http, reject embedded userinfo)
with `redactURLForError` so a persisted validation error can't leak a secret.
**Update** — Microsoft Ads MS-2.5 ad type corrected to Responsive Search Ad (PR #45
review, copilot — VERIFIED against learn.microsoft.com). The initial MS-2.5 added a
`TextAd`, but v13 does NOT support adding text/expanded-text ads (every `TextAd` field is
"Add: Not supported"; a standard-text-ad add fails with `CampaignServiceAdTypeInvalid`) —
the permissive test double masked a guaranteed runtime rejection. Switched the ad payload
to `ResponsiveSearchAd`: 3–15 unique headline assets (≤30) + 2–4 unique description assets
(≤90), each a `TextAsset` in an `AssetLink`, plus required `FinalUrls`. Ad group now sends
`AdGroupType: SearchStandard` (required to host an RSA) and a `Language` (campaign sets
none). `Ad.Status` defaults to Active on Add, so the ad sends `Status: Paused` explicitly.
`Ad.Type` "ResponsiveSearch" IS sent as the AddAds polymorphic discriminator (Add:Read-only
bars changing the type, not omitting the wire discriminator). `CampaignInput.Headline/Description` (singular)
became `Headlines/Descriptions` (lists); `composeAdCopy` de-dups/truncates/pads to the
minimum, `validateAdCopy` rejects over-count/over-long up front. Also: ad URL + copy now
validated BEFORE the campaign create (not before the ad group) so a bad input never
orphans a PAUSED campaign; `AlreadyExisted` is true only when all three levels pre-existed.

**Update** — Microsoft Ads MS-2.5 (ad group + ad) on the corrected v13 contract
(`adgroup_ad.go`). `CreateCampaign` now completes Campaign → AdGroup → Ad (all PAUSED).
Creates are `POST /AdGroups` (body `{CampaignId,AdGroups}`) and `POST /Ads` (body
`{AdGroupId,Ads}`) — parent id in the BODY, not the URL; reads are `POST
/AdGroups/QueryByCampaignId` and `POST /Ads/QueryByAdGroupId` (POST-with-body, not GET).
Ad-group idempotency = case-insensitive name; ad idempotency = FinalUrl match. Shared
`firstEntityID` classifier; reconcilable partials carry the ids known so far. Ad
destination validated (https/http, no userinfo) before any ad-group create; `FinalUrls` =
registration URL with LFX `utm_*` set; copy caller-supplied or derived from EventName,
bounded to Title 30 / Text 90. Rebased onto the corrected MS-2 (AccountId body, POST
QueryByAccountId lookup, case-insensitive-unique names, TimeZone sent).

**Update** — Microsoft Ads MS-2 corrected to the real v13 REST contract (PR #44 review,
copilot — VERIFIED against learn.microsoft.com). The initial MS-2 assumed a
GET-CampaignsByAccountId lookup, no request-body AccountId, and duplicate-names-allowed —
all wrong. Fixed: (1) the create body now carries the REQUIRED top-level `AccountId`
(AddCampaigns rejects every create without it); (2) the lookup POSTs
`Campaigns/QueryByAccountId` with `{AccountId,CampaignType}` in the body (the v13
GetCampaignsByAccountId REST op is POST-with-body, not GET); (3) campaign names are
CASE-INSENSITIVELY UNIQUE within the account, so findCampaignByName matches
case-insensitively and a duplicate-name PartialError
(`CampaignServiceCannotCreateDuplicateCampaign`) is surfaced as already-exists
(`isDuplicateCampaignNameErr`), not a clean failure; (4) `Campaign.TimeZone` is SENT
(defaulted) — the v13 Campaign object marks it deprecated but ALSO "Add: Required", so a
missing value would fail every create; (5) a null-only `PartialErrors` array is treated as
UNCONFIRMED, not a definite rejection, via `partialErrorsHaveAny` (v13's `PartialErrors` is a
SPARSE BatchError list — a failed item only, carrying an Index — so this is defensive handling
of a malformed null-padded body; the gate keys on an actual error code, not slice length). Tests rewritten to the real routes (assert AccountId in both
bodies, case-insensitive match, dup-name + null-PartialError handling, TimeZone present).
Client-side `parseErrorCodes` also now visits the v13 `BatchErrors` fault array (MS-1).

**Update** — Microsoft Ads campaign creation (MS-2, PR #44; LFXV2-2804).
`CreateCampaign` (in `internal/platform/microsoft/campaign.go`) find-or-creates a
PAUSED Search campaign. Two Microsoft quirks vs google-ads shape the contract:
(1) PartialErrors-on-200 — the create returns HTTP 200 with `{"CampaignIds":[id-or-null],
"PartialErrors":[...]}`, so `firstCampaignID` inspects the body and distinguishes a
definite rejection (null id + PartialError → clean failure) from a malformed 200 (no id,
no error → UNCONFIRMED). (2) Duplicate names REJECTED — Microsoft rejects a create whose
campaign name already exists (code 1115), which is what makes the deterministic name a
reliable idempotency key: `findCampaignByName` runs before the create, and a create that
loses the race to the 1115 self-heals by re-looking the winner up (mirroring the ad-group
1214 path); `CampaignsByAccountId` returns the full set (no pagination). Budget is
`DailyBudget`, a plain decimal in account currency (NO micros, unlike google-ads).
Review-hardened (PR #44, cursor + copilot): (a) a lookup failure is a clean `(nil, err)`
abort ONLY when the CALLER's context is done — the gate is `ctx.Err() != nil`, NOT
`errors.Is(err, DeadlineExceeded)`, because the client's per-attempt `context.WithTimeout`
can surface `DeadlineExceeded` while the caller context is still live (that case is an
UNCONFIRMED lookup failure, not a clean abort); (b) `TimeZone` is now sent — Microsoft
REQUIRES `Campaign.TimeZone` on create (NOT inherited from the account), defaulting to
`PacificTimeUSCanadaTijuana` when the caller doesn't supply one. `toMSDate({Month,Day,Year})`
is reserved for the ad-group flight dates a later slice needs.

**Update** — Added `internal/platform/microsoft`, the Microsoft Advertising (Bing Ads)
Campaign Management REST v13 client (MS-1 scaffold, PR #43; LFXV2-2804). Speaks REST
directly (not SOAP), mirroring the googleads client: OAuth2 refresh-token exchange vs
the Microsoft identity platform (scope `msads.manage offline_access`) + `DeveloperToken`
/`CustomerAccountId`/`CustomerId` headers, single-flight token cache, and the pre-send /
ambiguous / definite error-classification contract. Review-hardened (PR #43, cursor +
copilot): (1) status-aware read/oversize errors — a 2xx read failure is an ambiguous
`transportError`, a known non-2xx keeps its status as `apiError` (was a plain
`fmt.Errorf` that `createOutcomeAmbiguous` read as non-ambiguous, inviting a duplicate
create); (2) `transportError` cause UNEXPORTED + rendered via `safeCause` so a
`*url.Error` URL can't leak into a persisted step; (3) per-attempt
`context.WithTimeout(msAdsRequestTimeout)` so a `WithHTTPClient{Timeout:0}` can't hang;
(4) token fetched INSIDE the retry loop (a long 429 backoff could otherwise 401 the
resume); (5) over-cap `Retry-After` compared in seconds before the Duration multiply
(overflow → short-wait bug) and `parseNonNegativeInt` overflow rejected before wrap;
(6) single-flight concurrency test (leader + followers, cancel one mid-refresh, assert
one HTTP call) under `-race`. Registered the OKF concept + code index bullet.

## 2026-07-21

**Update** — HubSpot deep-review pass (PR #35). Ran a 5-dimension parallel review
(context/concurrency, error-classification, test-completeness, API-contract/docs, security)
with adversarial verification of each finding. One REAL bug + polish:
(1) SECURITY: `transportError` and `preSendError` had EXPORTED `Err error` fields holding a
`*url.Error` whose exported `.URL` carries the full request URL incl. `?after=<cursor>`.
`Error()` strips it via safeCause, but JSON/reflection serialization of the error (a
structured logger, error middleware) walks the exported field and leaks the cursor — the
exact vector the package already eliminated for `apiError` (no Body field). Unexported both
to `err` (like `unconfirmedError`); `Unwrap()`/`errors.Is/As` and the URL-free `Error()`
are unaffected. Added json.Marshal leak-regression assertions to both leak tests.
(2) Test coverage: mutating-3xx-is-UNCONFIRMED, connection-refused-is-preSend-NOT-unconfirmed
(dial+ECONNREFUSED → definite pre-send), ListEventDefinitions malformed-body + stuck-cursor
guards. (3) Fixed two stale test comments (`properties`→`includedProperties`,
`nextPageToken`→paging `after` cursor).

**Update** — HubSpot ctx-cancel pre-send guard (PR #35 review, copilot — a REAL
correctness bug, not a nit). `doRequest` fired `httpClient.Do(req)` even when the caller's
context was ALREADY done before send. A ctx cancellation isn't an `isPreSendDialError`, so
the resulting `Do` error fell through to `transportError{Mutating: !idempotent}` →
`IsUnconfirmed == true` — wrongly telling a caller of a MUTATING request that the mutation
MIGHT have committed (verify-before-retry) when nothing was ever sent. Added a `ctx.Err()`
guard right before `Do` (inside the retry loop, so it also covers a ctx that expires during
a 429 backoff), returning a clean `preSendError` (definitely-not-sent → `IsUnconfirmed ==
false`). Mirrors the established ctx.Err() pre-send guard in the googleads/reddit/meta
clients. Added `TestDoRequest_AlreadyCancelledCtxIsPreSendNotUnconfirmed` (asserts
preSendError, NOT unconfirmed, wraps context.Canceled, and the server is never hit).

**Update** — HubSpot FINAL review pass (PR #35). Ran an exhaustive self-review of the
whole hubspot diff (correctness, test-strength, doc-drift, API-contract, security) and
closed the last three things an automated reviewer could flag — the code was already
functionally correct: (1) `TestDoRequest_Mutating429IsNotRetried` now asserts
`IsUnconfirmed(err)` (a mutating 429 is ambiguous/may-have-committed), closing the pending
Copilot suggestion and catching an `Ambiguous`-flag regression. (2) Fixed the
`DefaultBaseURL` const comment that still showed `/crm/v3/lists/` with a trailing slash.
(3) Added clarifying comments on GetEmail/CloneEmail explaining the value-decode already
covers a null body via the `e.ID == ""` check (patchEmail uses the *Email-pointer pattern;
these don't need to). Branch frozen at this head for merge.

**Update** — HubSpot round-22 (PR #35 review, copilot). (1) `patchEmail`: a 2xx JSON
`null` (or empty) body decodes into the Email struct WITHOUT error (zero-valued), so the
id-fallback would report a PHANTOM success for a malformed response. A PATCH is mutating,
so a null/empty body is now UNCONFIRMED (the update may have applied). Added
`TestPatchEmail_NullBodyIsUnconfirmed`. (2) Doc: corrected the round-20 log entry that
still said `properties=…` (the code uses repeated `includedProperties`), and added a
missing blank line before a `**Creation**` heading that was folding two log entries.

**Update** — HubSpot round-21 (PR #35 review, cursor). (1) Switched the field restriction
from a CRM-style comma-separated `properties` string to REPEATED `includedProperties`
entries — the marketing-emails LIST endpoint uses that shape, not the CRM `properties`
convention. (2) The malformed-2xx guard (Results==nil) now fires on ANY page, not just
page 0: on a later page a missing results array would otherwise silently TRUNCATE the walk
and return a partial. Applied to SearchEmails + ListEventDefinitions. (3) Added the same
guard to SearchLists (Lists==nil → error; an empty search returns `{"lists":[]}`, non-nil).
Tests added for the SearchLists malformed case. (Also merged main to pick up #33.)

**Update** — HubSpot round-20 batch (PR #35 review, copilot + dealako). (1) RESTORED
`sort=-updatedAt` on SearchEmails — verified against HubSpot's v3 docs that `sort` IS a
valid GET /marketing/v3/emails param (round 13 wrongly dropped it; another bot flip-flop,
like objectTypeId). Client-side parsed-instant sort stays as the guarantee. (2) Added a
repeated `includedProperties` values for name, subject, and updatedAt — the list
endpoint returns FULL email content by default, so at limit=100 rich templates could blow
the response cap. (3) SearchEmails
and ListEventDefinitions now ERROR on a malformed 2xx (`{}`/`null` → Results==nil, no
paging) instead of returning a clean empty success that hides a broken response (an empty
portal returns `{"results":[]}`, non-nil, still returns 0). (4) Removed the dead
`cloneEmailRequest.Language` field (never populated; omitting language IS the
preserve-source-locale behavior). (5) Doc fixes: `private_app_token` is NOT a
`hubspot_connections` column (the token lives inside the encrypted creds blob) — corrected
client.go + internal-platform-hubspot.md; fixed a stale `/crm/v3/lists/` createListRequest
comment. (6) Tests: sort/properties assertions, malformed-body, and the id-tiebreak.

**Update** — HubSpot error-path query-strip + comment/test cleanup (PR #35 review round 18,
copilot). (1) `doRequest` now strips the query string from `path` before it reaches any
error (the full path is already in `u` for the URL) — a paginated request carries
`?after=<cursor>`, and the cursor (or any future query secret) must not leak through the
URL-free error contract. Added `TestDoRequest_ErrorPathStripsQueryString`. (2) Corrected
the stale `Email.UpdatedAt` field comment (it still claimed lexical sorting; the code
parses). (3) `TestSearchEmails_SortsByParsedInstantNotLexical` checks `len(got)` before
indexing so a pagination/filter regression reports the failure instead of panicking.

**Update** — HubSpot sort-by-instant + drop error-body snapshot (PR #35 review round 15,
copilot). (1) `sortEmailsByUpdatedDesc` now PARSES `updatedAt` as an RFC3339 instant
before comparing — a raw lexical compare mis-orders equivalent instants with different
offsets/fractional seconds (`2026-01-01T00:30:00+01:00` is OLDER than
`2026-01-01T00:00:00Z` but sorts lexically after). Missing/malformed → zero time (sorts
last); id tiebreak. Added `TestSearchEmails_SortsByParsedInstantNotLexical`. (2) Removed
`apiError.Body` + `readErrorSnapshot` entirely: nothing in this package classifies on the
body, and an EXPORTED Body field could leak upstream request material via reflection/JSON
serialization of the error even though `Error()` omits it. Non-2xx responses are now just
drained for connection reuse (googleads keeps a snapshot only because it parses error
codes from it).

**Update** — HubSpot remove dead Label field + doc fix (PR #35 review round 14, copilot).
(1) Removed the unused `AccountConfig.Label` — it was documented as "surfaced on results"
but this client's operations return raw Email/List objects with no result envelope to
carry it, and nothing read `c.account.Label` (the meta/reddit clients DO surface it on
their campaign-result types; hubspot has none yet). A no-op config field misleads callers,
so it's gone until there's a result type that reads it. (2) Doc: `CreateList` endpoint in
internal-platform-hubspot.md now shows the canonical no-trailing-slash `/crm/v3/lists`
(matching the round-13 code fix).

**Update** — HubSpot endpoint-contract fixes (PR #35 review round 13, copilot).
(1) `SearchEmails` — [CORRECTED in round 20: `sort` IS a valid GET /marketing/v3/emails
param per HubSpot's docs; this round wrongly dropped it. Round 20 restored `sort=-updatedAt`
as a server hint.] The most-recently-updated-first order is guaranteed CLIENT-SIDE
regardless via `Email.UpdatedAt` + `sortEmailsByUpdatedDesc` (parsed-instant compare, id
tiebreak) applied to the aggregated matches. (2) `CreateList` now
POSTs to the canonical `/crm/v3/lists` (no trailing slash) — HubSpot canonicalizes a
trailing slash via redirect and this client refuses redirects, so `/crm/v3/lists/` could
have produced a failed/ambiguous create. Updated the test path assertion.

**Update** — HubSpot cursor decode preserves `+` (PR #35 review round 12, cursor/copilot).
The round-10 `decodeCursor` used `url.QueryUnescape`, which converts a literal `+` to a
space — but base64 paging cursors legitimately contain `+`, so a token like `A+B/C=`
would be sent as `A B/C=` and break pagination. Switched to `url.PathUnescape`, which
decodes `%XX` while preserving `+`. Added `TestSearchEmails_PreservesPlusInCursor`. Also
fixed a stale `List.ObjectTypeID` field comment that still carried the (wrong) round-6
"objectTypeId is response-only" claim.

**Update** — HubSpot constructor input normalization (PR #35 review round 11, copilot).
`NewClient` now trims the injected `PrivateAppToken` and `PortalID` (mirrors meta/twitter):
a whitespace-only token is treated as missing (rather than sent as `Bearer   `), and a
padded portal id can't build invalid app URLs. Added `TestNewClient_NormalizesTokenAndPortalID`.

**Update** — HubSpot cursor decode + dedup clarity (PR #35 review round 10, copilot/cursor).
(1) HubSpot returns `paging.next.after` already percent-encoded (e.g. `MjA%3D`); feeding
it straight back through `url.Values.Encode` double-encoded the `%` (→ `MjA%253D`),
corrupting page-2 of `SearchEmails`/`ListEventDefinitions`. Added `decodeCursor`
(QueryUnescape-once, unchanged on non-encoded tokens, falls back to the raw token on
error) and use it in both cursor paginators. Added `TestSearchEmails_DecodesEncodedCursor`.
(2) Clarified `SearchLists` loop-detection: `seen`/`newThisPage` intentionally track the
RAW server rows independently of the contact filter (progress ≠ what we keep). (3) Fixed a
dangling doc-comment in `client_test.go`.

**Update** — HubSpot defensive filter tolerates omitted objectTypeId (PR #35 review
round 9, cursor). The round-8 client-side check dropped any hit whose `ObjectTypeID` !=
"0-1" — but a HubSpot response can OMIT `objectTypeId`, leaving it empty, which would
drop valid contact lists (the server-side filter already guaranteed they're contacts).
Now the defensive check drops a hit only if its type is EXPLICITLY non-contact (`ot != ""
&& ot != "0-1"`); an empty/omitted type is trusted. Test updated with an omitted-type
fixture row that must be kept.

**Update** — HubSpot contact-list filter RESTORED server-side (PR #35 review round 8,
copilot — REVERSES round 6). VERIFIED against HubSpot's official v3 docs:
`objectTypeId` IS a valid `ListSearchRequest` body field — the docs give the exact
example `{"query":"HubSpot","processingTypes":["MANUAL"],"objectTypeId":"0-1"}`. Round 6
had claimed the opposite (that it's a response-only property) and the server-side filter
was dropped in favor of client-side only; that was based on a wrong API claim from a
self-contradicting bot review. `SearchLists` now sends `objectTypeId: "0-1"` in the
request again (server-side filter), KEEPING the per-hit `ObjectTypeID` check as
defense-in-depth, and restored the body assertion (`objectTypeId == "0-1"`). The
`TestSearchLists_FiltersToContactListsClientSide` defensive test stays. NOTE for future
reviewers: this is settled against the HubSpot docs — do not remove the server-side
`objectTypeId` again.

**Update** — HubSpot dedup + cap coverage (PR #35 review round 7, cursor/copilot).
`SearchLists` (offset paginator) now tracks seen list ids and errors when a non-empty
page adds no NEW ids (server repeating a page), matching the cursor paginators'
stuck-cursor guard — previously it could return duplicate rows. Added a boundary test
for the 10 MiB response cap: a body AT the limit succeeds, limit+1 is a `transportError`,
and an over-cap MUTATING call stays `IsUnconfirmed`.

**Update** — HubSpot contact-list filtering (PR #35 review round 6, copilot). [SUPERSEDED
by round 8 — see the top entry.] This round removed the server-side `objectTypeId` filter
on a bot claim that it wasn't a valid `ListSearchRequest` field. That claim was WRONG
(HubSpot's docs document the field), so round 8 restored the server-side filter. What
survives from this round: `ObjectTypeID` was added to the `List` struct and a per-hit
client-side check + `TestSearchLists_FiltersToContactListsClientSide` were added — both
KEPT as defense-in-depth alongside the server-side filter.

**Update** — HubSpot input-normalization (PR #35 review round 4, cursor).
`SearchEmails`/`SearchLists` trim the query before matching/forwarding (a padded term
no longer silently returns no results), and `CloneEmail` trims `cloneName` and rejects
an empty-after-trim name (consistent with `CreateList`), so a padded name can't produce
a misnamed draft.

**Update** — HubSpot paginator hardening (PR #35 review round 3, cursor).
`SearchEmails` and `ListEventDefinitions` now error on a non-advancing cursor (a
repeated `paging.next.after` token) instead of re-fetching the same page until the cap
and duplicating results — matching the offset guard `SearchLists` already had.
`CreateList` trims its name before posting (padding no longer becomes part of the list
name).

**Update** — HubSpot client hardening (PR #35 review round 2, copilot/cursor).
(1) All id entry points trim-and-reassign before use (`GetEmail`,
`PatchEmailSettings`, `SetSendList`, `CloneEmail`, `GetList`, `UpdateListFilters`) —
a whitespace-padded id sent raw yields a 404/rejection that silently fails staging.
(2) `SearchLists` now errors on an empty page while `hasMore=true` instead of
returning a silent partial (a truncated audience list under-targets); the cap-exceeded
paths deliberately keep returning an error (all-or-error contract, never a silent
partial). (3) Corrected the `transportError` doc: it is ambiguous ONLY for a MUTATING
call (`IsUnconfirmed` returns `transportError.Mutating`); an idempotent read/search
that failed in transit is safely retryable.

**Update** — HubSpot client v3-contract fixes (PR #35 review, copilot; verified
against HubSpot's OpenAPI specs). (1) `PatchEmailSettings`/`SetSendList` now PATCH the
DRAFT route `/marketing/v3/emails/{id}/draft` — the base `/{id}` route mutates the
LIVE email, so draft edits were hitting the wrong endpoint. (2) `SetSendList` is now
ILS-ONLY: HubSpot's ILS migration removed functional support for the legacy
`contactLists` recipient field after 2024-10-31, so the client never emits it (dropped
the `isILS` param + the legacy numeric-id handling; callers resolve an ILS list id from
the Lists API). (3) `SearchLists` constrains results to
contact lists via the `objectTypeId "0-1"` request field (a valid `ListSearchRequest`
field — see the round-8 entry; a round-6 detour briefly moved this client-side before it
was restored server-side). It also drops the invalid
`includeFilters` search-body field, and reads
membership size from `hs_list_size` (a STRING under `additionalProperties`, requested
explicitly) — there is no top-level `size`, so `List.Size` was always 0. (4) A mutating
429/3xx/5xx `apiError` is now flagged `Ambiguous`; new `IsUnconfirmed(err)` lets callers
distinguish a may-have-committed outcome from a definite 4xx. (5) 429/error response
bodies are drained (bounded) before close so the keep-alive connection is reused on
retry. (6) Added multi-page pagination tests (cursor + offset forwarding, aggregation,
termination) for all three list-walkers.

**Update** — GA budget-name reconcile guidance qualified (PR #33 review, copilot). The
`campaignNamePartial` comment + `internal-platform-googleads.md` claimed the budget and
campaign names always DIFFER, so `CampaignBudgetName` is the budget reconcile key. That's
true only PRE-attachment: a non-shared (`explicitlyShared=false`) budget's name
SYNCHRONIZES to the campaign name once the campaign attaches, so at a campaign-stage
ambiguous failure the budget's current name is unknown (may be `campaignName`). The code
already handles this — the budget-stage partial (`budgetPartial`) carries
`CampaignBudgetID`, so past attachment reconciliation is by ID, not name — this just
corrects the comment/doc to say so (no behavior change).

**Update** — GA error-body snapshot no longer pins the full response (PR #33 review,
copilot). `doRequest` built `apiError.Body` as `string(raw)[:maxErrorBodyChars]` — the
400-char substring shared the up-to-`maxResponseBytes` backing array, so every retained
apiError pinned the whole body. Now the raw BYTES are sliced to the cap first and only
the bounded slice is converted to string (a fresh allocation), so the snapshot retains at
most `maxErrorBodyChars`. Error-code parsing still runs against the FULL raw body first,
so duplicate/field-error classification is unchanged. Added
`TestDoRequest_ErrorBodySnapshotIsBounded`.

**Update** — GA CampaignInput gains EventSlug (PR #33 review, dealako). Added a plumbed
`EventSlug` field to `googleads.CampaignInput` for struct parity with the meta/twitter/
reddit clients (which build UTM click-through params from it). GA's CreateCampaign builds
only a PAUSED shell today (no ad/final URL), so the field is accepted but not yet
consumed; GA-3+ ad creation will use it. Reserved now so the platform-agnostic input
shape stays stable.
**Update** — Made the create-brief + create-campaigns `project_id` SLUG-ONLY in the published Goa contract (PR #36 review, copilot). The handlers already reject a UUID at runtime (validateProjectSlug), but both create methods still declared `projectIDAttr` ("UUID or slug"), so generated/OpenAPI clients accepted UUIDs the handlers then 400'd. Added `projectSlugAttr()` (Pattern `^[a-z0-9]+(-[a-z0-9]+)*$` + MaxLength(35)) to those two methods and regenerated the API; read/update/delete stay `projectIDAttr` (UUID-or-slug; migration 000003 preserved historical UUID rows). Also tightened `projectSlugRe` to reject consecutive hyphens (`foo--bar`) so it matches the "single internal hyphens" contract; added foo--bar/cncf- to the rejection test. **Update** (same PR, later review): extended the SAME slug-only contract to ALL SEVEN connection-CREATE endpoints (`create-{provider}` via `connectionMethods`) — a connection is stored keyed by `project_id`, the exact-match key for the dispatch lookup, so a UUID-keyed connection could never join a dispatched campaign. `validateConnectionProjectSlug` guards each `Create*` service method (connections-flavored 400); the generated decoder validates the pattern too; get/update/delete/set-credential/test stay permissive for historical UUID rows. Compatibility-impacting: a UUID connection-create payload now 400s where it previously succeeded.

**Update** — PR #40 review (round 11): two fixes. (1) Archived-brief lifecycle
inconsistency (cursor): `ListAudiences` 404s on an archived parent brief, but
`GetAudience`/`UpdateAudience` only matched the audience row and never re-checked the
brief was active — so after archiving, list failed while get/patch still succeeded on
the same nested resource. Added an `EXISTS(active brief)` predicate to `GetAudience`'s
query (Update loads via Get, so the patch path is covered too), consistent with List +
Create. (2) Doc drift: `internal-infrastructure-postgres.md` still showed the old
`btrim(...) <> ''` 000006 constraint; updated it to the `~ '[^[:space:]]'` expression.

**Update** — PR #40 review (copilot, round 10, after David's approval): two fixes.
(1) UpdateAudience checked If-Match only via the repo's atomic write, AFTER the merge +
built-invariant Validate() — so a patch valid against the client's fetched version but
content-invalid once merged onto a NEWER stored version returned 400 instead of 412
(stale ETag). Added an explicit `cur.Version != version → 412` check right after
GetAudience (before merge/validate); the repo's atomic check still catches a read→write
race. Added a regression test (`TestAudienceService_Update_StaleIfMatchIs412NotContent400`).
(2) The built-invariant CHECK (000006) used `btrim(x) <> ''`, which strips only ordinary
spaces — a tab/newline-only master-list id passed the DB CHECK but `Validate()`
(strings.TrimSpace) rejects it. Switched to `platform_master_list_id ~ '[^[:space:]]'`
(requires a non-whitespace char), matching the app.

**Update** — PR #40 review (copilot, round 9): two fixes. (1) Cross-tenant integrity gap:
`campaign_audiences.brief_id` referenced only `campaign_briefs(id)`, so the copied
`project_id` was unchecked — a worker/backfill/direct write could persist an audience
whose `project_id` differed from its brief's, and `GetAudience` (trusts the stored
`project_id` for tenant scoping) could expose it under the wrong tenant. Added migration
000007: a composite FK `(brief_id, project_id) → campaign_briefs(id, project_id)` (plus
the `UNIQUE (id, project_id)` on campaign_briefs the composite FK requires). The API
create path already guarded this via `INSERT … WHERE EXISTS` an active project-scoped
brief; the FK makes the DB the source of truth for all writers. (2) Doc drift: updated
`cmd-campaign-service.md` to say `buildMux` mounts health/campaign, connection, brief,
AND audience servers (it said only health + connection).

**Update** — PR #40 human review (David CHANGES_REQUESTED + Rashad). Fixed the one
blocking defect: `CreateAudience` stored `created_by` as the JSONB literal `null` for an
unattributed row — `actorFromCtx` returns a typed-nil `*model.Actor` that slips past
`marshalAny(any)`'s `v == nil` guard (a typed nil boxed in an interface is not `== nil`)
and JSON-marshals to `"null"`. Added a `marshalActor(*model.Actor)` helper that checks
the concrete pointer, so no actor → SQL NULL. Also (agreeing with both reviewers) added a
DB CHECK `campaign_audiences_platform_valid` (`platform IN ('hubspot')`) to migration
000006 so the platform enum is datastore-enforced like `status`, not only at request
time. Clarified `audienceFromInput` status handling to an explicit if/else (behaviorally
identical — `StatusOrDefault()` was already a no-op when set — but a reviewer misread the
unconditional call as an overwrite; the false positive is now un-misreadable). Dropped
the dead `id` parameter from `audienceFromInput`. Added tests: nil-actor→NULL created_by,
and explicit-status-preserved-on-create.

**Update** — PR #40 follow-up review: two fixes. (1) The "explicit empty list clears
suppressions" contract couldn't round-trip: `suppression_list_ids` is an optional array,
so the generated client encodes it `json:"...,omitempty"` and a non-nil `[]string{}` is
dropped on the wire — the clear silently didn't work. Replaced the empty-slice signal with
an explicit `clear_suppression_lists` boolean in `AudienceUpdateInput` (always encodes;
takes precedence over a supplied list), regenerated `gen/`, updated `applyAudiencePatch`/
`hasAudiencePatch`, and added a service test for replace/clear/precedence. (2) `mapAudienceErr`
mapped `ErrNotFound` → "the audience was not found", but on create/list that error comes
from a missing/cross-project/archived PARENT BRIEF — made the shared message
resource-neutral ("the audience or its parent brief was not found").

**Update** — Route + authz for campaign_audiences (LFXV2-2783). Verified the audiences
endpoints need NO new gateway wiring: they nest under `/briefs/{briefId}/audiences`, so
the HTTPRoute `briefs(/.*)?` regex already forwards them and the single Heimdall
`project-api` rule (`/projects/:projectId/briefs/**`) already authorizes them on
`campaign_manager` (confirmed by running the RE2 regex against real audiences paths).
Added explicit audiences rows to the route/rule PARITY test (parity_test.go accepted
table) so a future narrowing of the briefs match/rule can't silently unroute or
de-authorize them, and documented the inheritance in api-catalog.md. No chart change.

**Update** — PR #40 follow-up review: two fixes. (1) `AudienceRepo.UpdateAudience` did
`UPDATE` then a SEPARATE `GetAudience` re-read to return the row — a race where a
concurrent version N+1 could land between the two statements and hand the first caller
the other writer's row + ETag. Switched to `UPDATE … RETURNING audienceCols` scanned
atomically, so the caller always gets the state its OWN write produced; the re-read
survives only on the no-row path to classify 404 vs 412 (it never becomes the returned
row, so it can't race). (2) Tightened the migration-000006 CHECK to reject blank/
whitespace master-list ids (`btrim(...) <> ''`), not just NULL — via the API empties are
written as NULL, but a direct/build-worker write could persist `''`, and the DB is meant
to be the source of truth for all writers.

**Update** — PR #40 review: updated `internal-container.md` to include the audiences
service in the no-DB and cold-start-503/late-binding mode enumerations (it was still
listing only connection + brief). The container wires `AudienceService` in all four
paths and late-binds it via `AudienceService.SetBackend` (same RWMutex/`ready()` pattern
as the brief service), so the OKF concept now matches the container behavior.

**Update** — PR #40 follow-up review: enforce the built-audience invariant. `AudienceBuilt`
is DEFINED as "the platform master list exists", but `status:"built"` was accepted with no
`platform_master_list_id` — persisting a row that claims a list its pointer is NULL. Added
`CampaignAudience.Validate()` (built ⇒ non-empty master-list id, evaluated on the EFFECTIVE
status) and call it before persisting on BOTH create AND update-after-merge, so no path (a
create with built+no-id, a status-only patch to built on an id-less row, or clearing the id
on an already-built row) can leave "built" meaning nothing — each is now a 400. Model +
service tests cover all three. Backed the app-level 400 with a DB CHECK constraint
(migration 000006: `status <> 'built' OR platform_master_list_id IS NOT NULL`) so the
platform build worker and direct writes can't violate it either — the datastore is the
source of truth, the API 400 a friendly early reject. (Reviewer-sim follow-ups: fixed a
godoc regression where `audienceValidationErr`'s doc comment detached `mapAudienceErr`'s;
documented the deliberate content-400-before-concurrency-412 precedence in UpdateAudience.)

**Update** — PR #40 follow-up review (two rounds): fixed the campaign_audiences PATCH
contract. (1) The update method reused `AudienceInput`, where `platform` is Required —
so the generated validator rejected a status-only/suppression-only patch unless the
caller also resent the immutable `platform`, defeating the "only supplied fields change"
contract. Added a dedicated `AudienceUpdateInput` (all mutable fields optional, no
`platform`), pointed `update-audience` at it, regenerated `gen/`, retyped
`applyAudiencePatch`. (2) But then every field being optional meant `{"audience":{}}`
passed the validator as a no-op that still bumps version/updated_at → invalidates other
clients' ETags → spurious 412s. Added a service-level `hasAudiencePatch` guard rejecting
an all-omitted patch as a 400 (with a test asserting the version is NOT bumped). Updated
the service tests to send platform-free patches and fixed the `AudienceInput` doc comment
(it is the CREATE payload; updates use `AudienceUpdateInput`). design.md notes the split.

**Update** — PR #40 review: extended the container startup tests to cover the new
audiences service (typed-503 in both no-DB and cold-start-503 modes + successful
`SetBackend` late-binding), and updated the architecture index for accuracy —
`design.md` now says four services and describes the audiences service, and
`api-catalog.md` gained a Campaign Audiences section listing the four nested routes.

**Creation** — Added the campaign_audiences Goa API (LFXV2-2782, epic LFXV2-2770) on
top of the existing DB layer (migration 000005 + model.CampaignAudience +
AudienceRepository + repo). `design/audience.go` defines the audiences service
(create/get/list/update) nested under a brief
(`/projects/{project_id}/briefs/{brief_id}/audiences[/{audience_id}]`), reusing the
shared design helpers (bearerToken/projectIDAttr/briefIDAttr/ifMatchAttr, JWTAuth,
the standard error set). Regenerated gen/ via goa. `internal/service/audience.go`
implements the handlers: maps payloads ↔ model, optimistic-concurrency update gated on
If-Match (same strong-validator parsing as briefs), ETag = version, typed error
mapping, and RWMutex `SetBackend` late-binding + typed-503 mode mirroring the brief
service. Wired into the container (no-db / 503-boot / live / cold-start-retry paths)
and mounted in the server (`buildMux` + a route-mount test asserting
`GET …/audiences` resolves non-404 + a nil-endpoints fail-loud case). Service-layer
tests cover create/defaults/If-Match(428/412/success)/404/late-binding. Full gate green.
**Update** — Made `page_id` + `account_id` REQUIRED and format-VALIDATED in
`MetaAdsConnectionConfig` (design/connection.go, PR #38 review, consolidated over three
rounds): an active Meta connection with an unusable id would always fail dispatch (the
Meta client rejects any `account_id` not matching `act_<digits>` and any non-numeric
`page_id` before a mutating call), so beyond `Required` we validate `page_id` with a
digits-only `Pattern` and `account_id` with `Pattern(^act_[0-9]+$)` — `Required`/
`MinLength(1)` alone would let `{"page_id":""}` or `account_id:"foo"` through. This
surfaces the error as a 4xx at connection creation instead of a silent runtime failure.
Added table-driven API-level tests exercising the GENERATED request-body validators
(missing/empty/non-numeric page_id and non-`act_` account_id rejected on both create and
update; valid numeric ids pass) — in a NON-generated package `internal/apivalidation`
that imports the exported validators, NOT under `gen/` (DO-NOT-EDIT boundary). Also fixed
a vacuous placement assertion in the meta dispatch happy-path test: it used lowercase
`facebook`/`instagram` JSON keys, but `meta.Placement` has no json tags so those were
silently ignored and the client applied its both-feeds default — switched to the correct
`FacebookFeed`/`InstagramFeed` keys and now assert instagram is ABSENT from targeting
(proving the `InstagramFeed:false` override is honored). Gave `CampaignCreateInput.
platforms` a deterministic UNIQUE `Example` ([reddit-ads, meta-ads] — two providers with
a registered dispatcher on this branch, so a consumer copying it doesn't hit "no
dispatcher registered") — Goa's auto-example otherwise repeated the first enum value
(duplicate `reddit-ads`), which the handler rejects. Regenerated the Goa API, dropped the now-non-pointer `cfg.PageID` deref in the
connection service, updated internal-dispatch.md, and strengthened the meta happy-path
dispatch test to assert the full mapping contract (objective→OUTCOME_SALES, lifetime
budget in minor units, geo countries, pixel + page promoted objects, per-variant
creative/ad fan-out).

**Update** — Added the linkedin and meta PlatformDispatcher adapters to
`internal/dispatch` (LFXV2-2638 / 2640), following the reddit template from the
Creation entry below. Each reuses the shared `credsSource` (Get → Decrypt) and does
its own per-platform interpretation: linkedin unmarshals a single OAuth2 accessToken +
builds RuntimeConfig from the connection's AccountID + numeric `org_id`; meta uses an
accessToken + AccountID (`act_...`) + `page_id`, budget in the account's currency (no
FX). Both are registered in `container.registerDispatchers` (fast path + cold-start
retry) alongside reddit — three of the paid providers. The twitter adapter (OAuth1
4-tuple, LFXV2-2642) is planned on a later branch and not yet registered. Each has
pre-create/NoUpstreamCreate tests + a happy-path through the real client against an
httptest server. Google Ads follows once its client (PR #33) lands; email/HubSpot is
LFXV2-2777.

**Creation** — Added `internal/dispatch` — the per-platform PlatformDispatcher
adapters that wire the orchestrator to the ad-platform clients (LFXV2-2639, Reddit
first). Until now the orchestrator's `dispatchers` map was empty, so campaign creation
recorded jobs that dispatched to nothing. The package has: a SHARED `credsSource`
doing the one mechanical step common to every platform (ConnectionReader.Get →
Encryptor.Decrypt, returning the raw plaintext + AccountID/ProviderConfig/Status) —
deliberately NOT interpreting the blob, since credential shapes differ per platform;
and a PER-PLATFORM `RedditDispatcher` that unmarshals its own `redditCreds` (OAuth2),
maps the brief's event fields + the per-platform `config` onto `reddit.CampaignInput`,
calls the client, and maps the result → `model.Campaign`. Claim contract: pre-create
failures (missing/invalid connection, config/credential errors, or a client `(nil,
err)`) are wrapped `notCreated` → a `preCreateError` implementing
`NoUpstreamCreate()`, so the orchestrator RELEASES the claim; ANY non-nil client
result + error (ambiguous create — the decision keys on result!=nil, NOT on a
populated id, since an ambiguous create returns a name-only partial whose id may be
empty) is handed back so the claim is RETAINED and the orphan recorded. Registered in
`internal/container`
(`registerDispatchers`, called from both the fast path and the cold-start retry path);
`logMissingDispatchers` warns for ad providers still without an adapter. Concept doc +
index added; dispatch/container/service tests green (-race).

## 2026-07-20

**Update** — Fixed "briefs stay broken after a cold-start DB retry" (PR #28 review,
cursor High, surfaced after #11 merged into #28). After #11 added the brief service +
orchestrator to the container, the 503-mode background retry only late-bound the
CONNECTION service + readiness — it never re-wired the BRIEF service, so brief/job
routes returned 503 for the whole pod lifetime while `/readyz` flipped to healthy
(readiness OK but routes 503 — worse than "unavailable"). Fixed: (1) gave
`BriefService` a `SetBackend(briefs, campaigns, jobs, orch)` late-binding setter
guarded by an RWMutex, with handlers now snapshotting collaborators via `ready()`
(so a mid-request swap can't race); (2) the retry goroutine now fully re-wires — brief
`SetBackend` + orchestrator + `FailStuckJobs` + `StartRecoverySweeper` — and flips
readiness LAST so `/readyz` never reports OK while brief routes still 503; (3) 503-mode
boot now wires a nil-repo brief service (routes mounted → typed 503, not a nil panic).
Added `TestBriefService_SetBackend_LateBinding` + a container 503-mode assertion.
Race-clean.

**Update** — Documented the Traefik `RegularExpression` HTTPRoute version requirement
(PR #28 review, copilot). Copilot claimed Traefik's Gateway API provider doesn't
support `RegularExpression` path matches (only Exact/PathPrefix) → the project-nested
route would be silently unrouted. VERIFIED WRONG against Traefik's source
(`buildPathRule`, every v3.1.0+ tag): a `RegularExpression` match is translated to a
native `PathRegexp(...)` rule (RE2/Go-regexp), GA, not gated. BUT two real nuances:
(1) **v3.0.x does NOT support it** (returns "unsupported path match"), so it requires
Traefik >= v3.1.0 — now stated in the template comment + concept doc; (2) the feature
is NOT in Traefik's Gateway API conformance report even though the code implements
it, so the render alone doesn't prove routing — added a note to verify the deployed
HTTPRoute's `Accepted` status condition is True. Replaced the vague "custom
conformance" wording. No route change (works on the platform's v3.1.0+ gateway).
NOTE: no other LFX service uses RegularExpression HTTPRoute (query-service uses
PathPrefix/Exact) because they route on their own top-level prefix; campaign-service
can't (project-service owns /projects/), hence the regex.

**Update** — Corrected the "re-run after a partial migration is harmless" doc claim
(PR #28 review, copilot). The container concept doc and the `Migrate` doc comment
said migrations are idempotent so a re-run after a partial is harmless — but that's
wrong for a PARTIAL (dirty) migration: golang-migrate marks the schema dirty
precisely because partial migration SQL is not assumed idempotent, and a re-run then
hits `ErrDirty` (needs manual `force`, exactly the permanent-failure path documented
above). Reworded both to scope the "skipped/harmless" claim to a CLEAN schema and
describe partial failure as the dirty/manual-recovery state.

**Update** — Fail fast on a PERMANENT migration failure instead of 503-looping
forever (PR #28 review, copilot + cursor). The 503-mode retry loop retried
`initDatabase` on ANY error — so a dirty schema (`migrate.ErrDirty`, set when a prior
migration failed partway) would loop forever behind a 503, with no fail-fast signal.
A dirty schema can't clear by re-running Migrate; it needs an operator to force the
version. Added `postgres.IsPermanentMigrationErr` (classifies a wrapped
`migrate.ErrDirty`); the synchronous fast path now returns an error (process exits
loud) and the background retry loop logs ERROR + stops looping on it. Connectivity /
lock / deadline failures are deliberately still transient (they retry). Note: the
overlapping-migration half of these findings was already fixed earlier (migrateMu +
pool-first-then-Migrate); these older bot comments predate that. Test added.

**Update** — Made the pgx DSN-parse errors DSN-free (PR #28 review, copilot). Both
`NewPool` and `ValidateMigrationDSN` wrapped `pgxpool.ParseConfig`'s error with `%w`;
NewContainer propagates it and main logs it, so a malformed credential-bearing
DATABASE_URL risked logging the connection string. VERIFIED that pgx's
`ParseConfigError` already redacts the password (`redactPW`) across every malformed
DSN shape I probed (bad port, space-in-host→url.Parse-fails-falls-to-keyword-regex,
bad connect_timeout/sslmode, keyword form) — so the finding's literal "leaks the
password" claim is not currently true. BUT we shouldn't depend on a dependency's
best-effort redaction for a secret, so wrapped both sites in a `dsnParseError` whose
Error() renders a STATIC DSN-free message and whose Unwrap() keeps the pgx cause for
errors.Is/As + diagnostics. Test asserts a password/DSN never reaches Error() while
the cause stays unwrappable.

**Update** — Added the route/rule PARITY test (PR #28 review, copilot). The PR
described an RE2 route/RuleSet parity regression guard, but none was committed — the
HTTPRoute regex and the Heimdall RuleSet path list are two hand-maintained matchers
with nothing coupling them, so a drift (a forwarded-but-unruled path) would skip the
campaign_manager FGA check unnoticed. Added `TestRouteRuleSetParity`
(`charts/lfx-v2-campaign-service/parity_test.go`): renders both templates via `helm
template`, extracts the RE2 regex + the RuleSet's project-nested patterns (translating
Traefik `:projectId`/`*`/`**`), and asserts a curated accepted/rejected path table
matches identically in both matchers (skips if helm absent; fails on render error).
Verified non-vacuous by flipping an expectation. httproute concept doc updated.

**Update** — Scoped the parity test to the campaign_manager rule (PR #28 review,
copilot). `extractRulePatterns` treated ANY `/projects/` path anywhere in the RuleSet
as "authorized", so a path moved into an allow_all/deny_all/differently-scoped rule
would still satisfy parity — but the actual invariant is campaign_manager on
project:{projectId}, not just "some rule matches". Now extraction is scoped to the
`project-api` rule BLOCK (isolated from its `- id:` to the next), and a new
`TestProjectAPIRuleEnforcesCampaignManager` (also called from both parity tests)
asserts that rule's authorizer is openfga_check with relation campaign_manager +
object project:{projectId}. A rule downgrade/re-scope now fails the security test.

**Update** — Strengthened the parity test to couple to matcher CONTENT (PR #28
review, copilot). The curated table only sampled fixed paths, so a one-sided
matcher edit that no case exercised (copilot's example: adding `tiktok-ads/metrics`
to the route regex only) would still pass. Added `TestRouteRuleSetParityWitnesses`:
it enumerates concrete example paths from the route regex's AST (`regexp/syntax`
walker — one witness per alternation leaf, `[^/]+`/`.*` collapsed to literals) and
requires each to be RULED, and builds a witness from every RuleSet pattern and
requires the route to FORWARD it. A route-only new branch now yields an unruled
witness → fail; a RuleSet-only entry yields an unforwarded witness → fail. Verified
against copilot's exact scenario (`/projects/x/tiktok-ads/metrics` is caught).

**Update** — Bounded the migration step with the startup deadline (PR #28 follow-up
review, cursor Medium). After the earlier pool-first fix, `initDatabase` still ran
`postgres.Migrate` (no context) synchronously with no time bound, so a reachable
but slow/lock-blocked migration could block `NewContainer` indefinitely. Now
Migrate runs in a goroutine under a package `migrateMu` (serializes runs so a retry
never starts a second migration while a prior deadline-abandoned one is finishing)
and the caller returns on the startup deadline. Also cleaned a union-merge artifact
in this log (duplicated oversized-body line).

**Update** — Hardened the #28 503-mode cold-start fix after review (cursor HIGH +
copilot). (1) `initDatabase` started `postgres.Migrate` (uncancellable Up()) in a
goroutine and returned on the 15s deadline WITHOUT waiting — so the retry loop
launched another migration while the previous was still blocked, leaking goroutines
and racing concurrent migrations. Reworked to open the pool FIRST (NewPool does a
context-bounded Ping) and run Migrate only after a reachable ping, so Migrate never
blocks against a down DB and retries never overlap. (2) A malformed DATABASE_URL
(keyword DSN) is deterministic, so `NewContainer` now fails fast via
`postgres.ValidateMigrationDSN` instead of 503-looping forever. (3) Corrected the
service.go comments/doc that claimed a NIL readiness dep makes /readyz not-ready —
a nil dep is treated as READY (no-DB mode); cold-start uses the non-nil notReady{}
checker. (4) The connection 503 message "not configured" → "unavailable" (during
cold start the DB is configured, just unavailable). Tests + concept doc updated.

**Update** — Made the DB cold-start startupProbe budget real (PR #28 review,
LFXV2-2558). `NewContainer` capped migration+pool init at 15s and `main` exited
on failure, so an unreachable DB at boot crash-looped the pod and the ~90s
startupProbe budget never applied. Now a *transient* DB-init failure boots the
services in 503 mode (a `notReady` health dep so `/readyz` returns 503, distinct
from no-DB mode; connection service nil-repo) and a background goroutine retries
migration/pool, swapping the live pool/repo in via `SetReadinessDep`/`SetBackend`
(mutex-guarded against concurrent request reads) once it opens. Config errors
(invalid DB settings, bad encryption key) still fail fast. `Close` cancels the
retry goroutine. Updated the container + deployment concept docs and the
startupProbe comment.
**Creation** — Added the `campaign_audiences` resource — DB layer (LFXV2-2773 subtask
2781, email epic LFXV2-2770). Migration `000005` creates `campaign_audiences` (a built
audience subordinate to a brief: `brief_id` FK to `campaign_briefs`, columns store a
POINTER + provenance — `platform_master_list_id`, `suppression_list_ids`,
`inclusion_summary`, `status` building/built/failed, `version` — NOT the audience
contents, which stay in HubSpot). This is the "B2" decision: a built audience is a
first-class, inspectable, reusable, versioned LFX resource. Added `model.CampaignAudience`
(+ AudienceStatus, StatusOrDefault), `domain.AudienceRepository` interface, and
`postgres.AudienceRepo` (create/get/list/update; project-scoped; optimistic-concurrency
update gated on version → ErrPreconditionFailed, matching ReplaceCampaign). Indexed on
brief_id + project_id (no natural uniqueness — a brief may have many audiences). The
Goa API/handlers + route/rule wiring are the sibling subtasks (2782/2783); the repo
isn't consumed until the service exists. Model unit test added; per repo convention
(no DB unit tests here — repos are covered via service-layer fakes) the migration is
validated on boot. Whole-module build/vet/test green; concept doc + log updated.

**Update** — Idempotency-lookup errors no longer silently fall through to dispatch
(PR #11 review, cursor Medium). In `dispatchPlatform`, the fast path treated ANY
non-nil error from `GetCampaignByPlatform` like "no row" and fell through to
claim/dispatch — so a transient/real DB failure that hid an existing campaign could
trigger a duplicate upstream create, with no log/signal. Now the outcomes are
distinguished: existing-with-upstream-id → reuse; `ErrNotFound` → fall through to the
claim; any OTHER error → surface as a platform failure (logged ERROR), not a blind
dispatch. Corrected the concept doc, which had documented the old swallow-the-error
behavior as intentional. Test added (`TestOrchestrator_IdempotencyLookupErrorIsFailure`).

**Update** — Addressed dealako's 4 [minor] review items on PR #11 (LFXV2-2626).
(1) `GetCampaignByPlatform` was the one campaign_repo method not scoped by
project_id — added a `projectID` param + `AND project_id=$3` (matching
GetCampaign/ClaimCampaignDispatch) for tenant-isolation defense-in-depth; updated
the domain interface + the orchestrator call site. (2) The rare double-fault in
`ClaimCampaignDispatch` (post-insert read AND rollback both fail) orphans a
`status='pending'` row that permanently blocks the (brief,platform) pair — now
logs at ERROR with project_id/brief_id/platform/job_id for alerting/manual
reconcile. (3) Added `TestClaimCampaignDispatch_ConcurrentSingleWinner` — N
goroutines racing the claim path, asserting exactly one wins and losers cleanly
no-op (the prior claim tests were single-threaded). (4) `design/brief.go`: `Brief`
now `Reference()`s `BriefInput` for the 8 shared attributes instead of
duplicating them — this also fixed a latent drift the manual copy had already
caused (Brief's `program_type` was missing BriefInput's Enum, so the generated
OpenAPI had no enum + gibberish examples on the Brief response; regenerated).
**Update** — Closed the second half of the X/Twitter URL leak (PR #31 review,
copilot). The transportError fix covered the AMBIGUOUS branch, but the PRE-SEND
branch (`isPreSendDialError` → DNS/connect-refused) still did a raw
`fmt.Errorf("... %w", err)` of the `*url.Error`, so a DNS/refused failure on a
create still rendered the request URL (X puts create params in the query string)
into persisted Steps. Added a `preSendError` type mirroring transportError's
URL-free `Error()` (via `safeTransportCause`) but semantically DEFINITE (request
never sent → not applied, unlike ambiguous transportError); `Unwrap()` retains the
cause so `isPreSendDialError`/`errors.Is` still match. Test added
(`TestPreSendError_DoesNotLeakURL`). NOTE: reddit/meta (merged) have the SAME raw
`%w` pre-send render — same follow-up as the transportError leak applies there.

**Update** — Fixed a URL leak + stale docs on the X/Twitter client (PR #31 review,
cursor Medium + dealako + copilot). (1) `transportError.Error()` rendered `%v` of
the wrapped `httpClient.Do` error — typically a `*url.Error` embedding the full
request URL (which can carry request material / a destination's secret query) —
and that string was copied into `PromotedTweetWarning` + persisted `Steps`. Added
`safeTransportCause` which unwraps a `*url.Error` to its underlying cause
(timeout/EOF/reset) with no URL; `Error()` now renders method/path + that. Test
added. NOTE: reddit/meta (merged) have the same `%v` transportError render —
follow-up to apply the same URL-suppression there. (2) Corrected the stale
`createOutcomeAmbiguous` header comment that still claimed "NOT gated on the HTTP
method" after the 3xx gate was re-added. (3) Documented CreateCampaign's
non-standard `(non-nil result, non-nil error)` contract so callers inspect the
result on error (for reconcile) instead of discarding it.

**Creation** — Added the `internal/platform/hubspot` Go package (email-channel
scaffold, LFXV2-2778 under epic LFXV2-2770). HubSpot's auth is the simplest of any
client — a STATIC private-app bearer token (no OAuth token-exchange flow), attached
directly by `doRequest`. The request layer mirrors the googleads/reddit/meta/twitter
discipline: no-follow redirects (fresh-client rebuild so a `WithHTTPClient` caller
isn't mutated), bounded 10 MiB reads, typed `apiError` (method/path/status only,
body never surfaced) + `transportError` (URL-free via `safeCause`, cause retained
via Unwrap), `isPreSendDialError` pre-send classification, and 429 retry gated on an
explicit `idempotent` flag (a non-idempotent create is never retried — no idempotency
key → double-create risk). Concept doc + code index added.

**Creation** — Added the HubSpot marketing-email ops (LFXV2-2779) + CRM-list/event-def
ops (LFXV2-2780) on the client. `email.go`: SearchEmails/GetEmail (idempotent),
CloneEmail, PatchEmailSettings, SetSendList. `lists.go`: SearchLists, GetList
(includeFilters=true → filterBranch + processingType), CreateList (DYNAMIC,
objectTypeId 0-1, opaque filterBranch), UpdateListFilters (PUT …/update-list-filters),
ListEventDefinitions. Creates/clones are non-idempotent; a 2xx-with-no-id is
UNCONFIRMED (a resource may exist → verify, don't blind-retry). SetSendList sets
recipients via `contactIlsLists` (ILS list ids) ONLY — HubSpot removed the legacy
`contactLists` recipient field after 2024-10-31 (see the 2026-07-21 ILS-only update),
so the client never emits it. Sends a complete `to` (contactIds cleared) with the ILS
send list + its suppressions. filterBranch shape invariants stay with the
audience-builder (LFXV2-2774), not this client. Full gate green.

**Creation** — Added the `internal/platform/snowflake` Go package (email channel,
LFXV2-2772 under epic LFXV2-2770): a READ-ONLY Snowflake client that resolves
past-edition EVENT_NAME/EVENT_ID from `ANALYTICS.PLATINUM_LFX_ONE.event_registrations`
for HubSpot BEHAVIORAL_EVENT filters. Read-only BY CONSTRUCTION — no arbitrary-SQL
entry point (unlike the reference app's `snowflake_query(sql)`); the one method
`ResolvePastEventNames` builds a fixed, fully-parameterized SELECT DISTINCT (terms
bind as ILIKE ?/NOT ILIKE ?, never interpolated; identifiers are constants guarded by
`ident`; LIMIT-capped). Source is PLATINUM (not the reference's Silver_Segment).
Fail-closed on error/empty (callers must NOT substitute guessed names). Key-pair (JWT)
auth via injected PKCS8 PEM, with `.env`-mangling tolerance (quotes/`\n`/CRLF); pool
opens lazily; DSN never quoted into errors. Tested with a hand-rolled in-process
database/sql driver fake (no new test dep) — 9 cases asserting query shape,
injection-safety, fail-closed, and key parsing. **DEPENDENCY:** adds
`github.com/snowflakedb/gosnowflake` v1.19.1 (the only official Go Snowflake driver;
no shared Go Snowflake service exists — the LFX One UI's Snowflake service is
TypeScript). Concept doc + code index added; `go mod tidy` run.
**Update** — Two more GA-2 partial/pre-send fixes (PR #33 review, copilot). (1) The
ambiguous/duplicate BUDGET partial exposed only `CampaignName`, but the resource that
may exist is a budget created under a DIFFERENT name (`LFX | Budget | …`) — with no id
yet, a caller couldn't reconcile it. Added `CampaignBudgetName` to `CampaignResult`
and populated it in every partial. (2) A pre-send contract hole: with a CACHED OAuth
token, an already-cancelled context reached `httpClient.Do`, got wrapped as a
`transportError`, and was reported UNCONFIRMED — but nothing was sent, so it's a clean
failure. Added an explicit `ctx.Err()` check immediately before the first mutate →
`(nil, err)`. (Without a cached token the token fetch surfaced the ctx error pre-send
anyway; the cached-token path reaches Do directly, hence the explicit guard.) Tests
added for both (the pre-send test warms the token cache first).

**Update** — Added `networkSettings` to the GA-2 SEARCH campaign create (PR #33
review, copilot — verified against v23 docs before applying). A SEARCH campaign that
targets NO network is rejected with
`CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK`, and an omitted
`networkSettings` resolves to exactly that (proto3 bools default false) — Google
documents no protective default and every official create sample sets it. The
rejection lands on `campaigns:mutate` AFTER the budget commits, so it would orphan the
budget. Now sends `networkSettings{targetGoogleSearch: true, targetSearchNetwork:
false, targetContentNetwork: false}` — Google Search only (conservative for a PAUSED
broker shell; targetSearchNetwork=true would require targetGoogleSearch AND opt into
Search Partners). Happy-path test now asserts the networkSettings block. Concept doc
updated.

**Update** — Corrected the GA-2 name-length limits after re-verifying the v23 docs
(PR #33 review round 3, copilot — TWO contradictory claims: one said 255, one said
128; BOTH wrong for Campaign). Authoritative from the v23 System Limits table + RPC
field refs: `Campaign.name` = up to **256 CHARACTERS** (`StringLengthError.TOO_LONG`);
`CampaignBudget.name` = **1..255 UTF-8 BYTES** (trimmed). Different number AND unit.
My earlier "128 chars" campaign cap was simply wrong (over-strict, rejecting valid
names). Fixed: `maxCampaignNameRunes=256` (validated via `utf8.RuneCountInString`),
`maxBudgetNameBytes=255` (validated via `len`); `validateEntityName` now takes the
measured length + unit label so each name is measured in its correct unit (a
multibyte name would otherwise slip past the budget's byte ceiling). Also confirmed
v23 forbids NUL/LF/CR in `Campaign.name` — already handled by the control-char
stripping in `sanitizeNamePart`. Replaced the 128-overflow test with a byte-limit
preflight test + a units (bytes-vs-runes) test. LESSON: when two AI reviewers give
contradictory numbers, verify against the primary source before implementing either.

**Update** — Fixed several GA-2 correctness bugs from PR #33 review (copilot +
cursor), verified against the v23 docs: (1) campaign create now sets the REQUIRED
`containsEuPoliticalAdvertising: DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` —
omitting it fails every create with FieldError.REQUIRED (and since 2026-04-01 an
undeclared account has ALL mutates rejected), which would have orphaned the budget.
(2) The campaign duplicate check used `DUPLICATE_NAME` (the BUDGET code); campaigns
use `CampaignError.DUPLICATE_CAMPAIGN_NAME` — split into isDuplicateBudgetNameErr /
isDuplicateCampaignNameErr so the campaign branch actually fires. (3) A mutating
429 is now UNCONFIRMED (doRequest suppresses its retry precisely because it may
have committed — was mis-classified as a clean failure → double-create risk). (4)
Error codes are now parsed from the FULL body in doRequest and retained on
`apiError.ErrorCodes`; hasErrorCode reads that field instead of re-parsing the
truncated `Body` (a real error JSON exceeds maxErrorBodyChars, so the old on-demand
parse of the truncated snapshot silently dropped codes, breaking all duplicate
detection). (5) A ctx check between the budget and campaign mutates skips the
campaign create on a done context, returning the budget as a reconcilable partial.
(6) Clarified docs: a campaign-create 4xx doesn't mean nothing was created (the
budget exists); the non-shared-budget name-reuse-on-retry corollary is undocumented
so retry-safety relies on a stable NameSuffix. Concept doc + index updated (GA-1→GA-2).

**Update** — Second GA-2 review round on PR #33 (5 fixes): (1) split the name-length
limit into `maxBudgetNameLen=255` / `maxCampaignNameLen=128` and validate each name
against its own limit — v23 permits a 255-char budget name but only 128 for a
campaign, so the collapsed single limit let a 129–255-char campaign name pass
preflight and get rejected by the paid campaigns:mutate AFTER the budget was
created (avoidable orphan). (2) Require BOTH Project AND EventName independently (was
either-or): Project is the attribution key the pipeline parses from the name, so a
one-segment name is mis-attributed. (3) Added `sanitizeNamePart` to strip the `|`
delimiter from caller segments before composing — a raw `|` would inject extra
pipe-fields and break name-based reconciliation/attribution. (4) `firstResourceName`
now returns (resourceName, id) and errors on a present-but-MALFORMED resourceName
(no id segment, e.g. `customers/1/campaigns/`) → UNCONFIRMED, instead of continuing
with an empty unreconcilable id. (5) Fixed the RejectsBadInput test (its budget
cases now set Project+EventName so they exercise the budget checks, not the new
attribution checks that run first) + added tests for the 128-overflow, pipe-strip,
malformed-resourceName, and firstResourceName cases. Concept doc updated.

**Update** — GA-2 PR #33 follow-up (copilot): renamed `CampaignInput.BudgetUSD` →
`Budget` (and `maxBudgetUSD` → `maxBudget`). Google applies `amountMicros` in the ad
account's OWN currency and this client does no FX conversion, so the `USD` suffix
was a false promise — 50 on a EUR account is 50 EUR/day, not ~54. Field comment now
states it's account-currency (NOT USD), and the budget-created step no longer
hardcodes a `$` sign. Mirrors the meta client, which renamed the same field for the
same reason. No behavior change (the value was already sent as-is).

**Update** — GA-2 PR #33 follow-up (cursor Bugbot): the both-fields-required check
validated the RAW input (`strings.TrimSpace`), but composeName only includes a
segment when its `sanitizeNamePart` is non-empty — so a delimiter-only value like
`"|||"` passed validation yet sanitized to nothing, dropping the Project segment
while still creating a paid budget/campaign. Fixed by validating the SANITIZED
value (`sanitizeNamePart(in.Project/EventName) == ""`) so validation and
composition stay consistent; added pipe-only test cases.

**Creation** — Added Google Ads campaign creation (GA-2, LFXV2-2637) in
`internal/platform/googleads/campaign.go`: `CreateCampaign` creates a PAUSED SEARCH
campaign as two sequential `:mutate` calls — a non-shared STANDARD `campaignBudget`
(amountMicros = budget×1e6) then a `campaign` referencing it with a `manualCpc {}`
bidding strategy. Both resource ids surfaced. Because `:mutate` has no idempotency
key, added `createOutcomeAmbiguous` (5xx/transport ambiguous always; 3xx only on a
mutating method) + `isDuplicateNameErr` (4xx DUPLICATE_NAME → already-exists) +
machine-readable error-code parsing (`error.details[GoogleAdsFailure].errors[].errorCode`,
body never surfaced, codes bounded): an ambiguous or 2xx-no-resourceName outcome →
UNCONFIRMED + reconcilable partial (carries the budget id once created); a definite
4xx → clean failure. Deterministic composed names so a retry collides on
DUPLICATE_NAME rather than double-creating. Table-driven httptest coverage for
every branch. Concept doc updated.
**Update** — Extended the Meta ad-set ambiguity to the 2xx-no-id case (LFXV2-2641,
PR #30 review by Copilot). The ad-set create's error path already routed through
`createOutcomeAmbiguous`, but a 2xx response with an empty `id` fell through to a
definite "returned no ad set ID" — the same duplicate-create risk as the campaign
and twitter no-id paths. Now surfaces UNCONFIRMED (verify before retrying). Test
added. Also fixed a CI `check-fmt` failure (gofmt comment alignment in the meta
test).

**Update** — Extended the X/Twitter create-outcome ambiguity to the INITIAL
CAMPAIGN create (LFXV2-2642, PR #31 review by Cursor + Copilot) — the last
uncovered create step. The campaign POST returned a bare `(nil, err)` on an
ambiguous 3xx/5xx/transport failure and a plain error on a 2xx-no-id, discarding
the deterministic campaign name; X may have committed the PAUSED campaign, so a
caller got no reconcile signal and could retry into a duplicate. Now returns a
name-carrying partial result + UNCONFIRMED (verify before retrying) for both cases
(a definite 4xx/pre-send error still returns plain `(nil, err)`), mirroring the
meta/reddit clients' name-only partial for the first create step. The whole
twitter flow (campaign → line item → promoted tweet) now classifies every create
outcome consistently. Tests added.

**Update** — Extended the X/Twitter create-outcome ambiguity to the LINE-ITEM
create (LFXV2-2642, PR #31 review by Cursor). The line-item POST always returned a
definite "line item creation failed" (even on a 5xx/mutating-3xx/transport error
where X may have committed it) and a definite "returned no line item ID" on a
2xx-no-id — the same blind-retry/duplicate risk already fixed for the campaign,
promoted-tweet, and meta ad-set paths. Both now surface UNCONFIRMED (verify before
retrying) when ambiguous; a definite 4xx/pre-send error still reads "failed".
Also updated the `PromotedTweetWarning` field contract (it told consumers the
promoted tweet "may need to be added manually", which for an UNCONFIRMED outcome is
the duplicate risk this exists to prevent — now it requires verifying before adding
or retrying) and corrected the twitter concept doc's "shallow copy" wording to the
fresh-client construction.

## 2026-07-19

**Update** — Fixed an http.Client copy-after-use in the Meta client's no-follow
enforcement (LFXV2-2641, PR #30 review by Copilot). `NewClient` value-copied a
`WithHTTPClient`-supplied client (`hc := *c.httpClient`) to override CheckRedirect
— but an `http.Client` must not be copied after first use (the copy duplicates its
internal mutex while sharing the request-cancellation map, so concurrent use of
the caller's client and the copy can race). Now builds a FRESH `*http.Client`
carrying only the exported reusable fields (Transport, Jar, Timeout) with
`CheckRedirect: noFollow`. The no-follow test asserts Transport/Timeout are
preserved and the fresh client is a distinct pointer. Also made the campaign
UNCONFIRMED step reason-neutral ("ambiguous response — timeout, server error, or
an unfollowed redirect") since a 3xx now routes there too. NOTE: the reddit client
(merged) has the same value-copy pattern — follow-up tracked to apply the same
fresh-client fix there. The twitter client gets the same fix on PR #31.

**Update** — Closed two more Meta ambiguity gaps (LFXV2-2641, PR #30 review by
Copilot). (1) `doRequest` returned a plain error when a NON-2xx response body
failed to read, stripping the HTTP status — so a mutating 3xx/5xx with an
unreadable body (the create may have committed) was mis-seen as a definite failure
by `createOutcomeAmbiguous` (which keys on the `*APIError` status). It now returns
an `*APIError` preserving the status on a non-2xx read failure (2xx read failures
stay `transportError`). (2) The ad-set create returned its error directly without
the ambiguity check the campaign and ad/creative creates use, so a surfaced 3xx/5xx
read as a definite "ad set creation failed" — risking a duplicate ad set on retry.
It now routes through `createOutcomeAmbiguous`: ambiguous → UNCONFIRMED (verify
before retrying), definite 4xx → "failed". Tests added for both. (3) The same
status-stripping existed in the OVERSIZED-body branch (> maxResponseBody, 10 MiB), which returned a
plain error before recording the status — a mutating 3xx/5xx over the cap was still
mis-classified as a definite failure. Now the oversized-body branch preserves the
status the same way (2xx → transportError, non-2xx → *APIError), with a regression
test. Updated the meta concept doc to describe the fresh-client + status-preservation.

**Update** — Gated the Meta client's 3xx create-outcome ambiguity on a mutating
method (LFXV2-2641, PR #30 review by Cursor Bugbot). `createOutcomeAmbiguous`
treated EVERY 3xx as UNCONFIRMED without checking the method, diverging from the
reddit client (which gates 3xx on `isMutatingMethod`) despite claiming to mirror
it. All call sites pass POST today so behavior was unchanged, but the helper's
contract was wrong for any future GET caller — a GET redirect is not a create.
Added `isMutatingMethod` to the meta client and gated the 3xx branch (5xx and
transport errors stay ambiguous regardless of method); extended the ambiguity test
with GET/POST/DELETE method cases. Now genuinely identical to reddit.

**Update** — Fixed the http.Client copy-after-use in the X/Twitter client's
no-follow enforcement (LFXV2-2642, PR #31), matching the meta fix (PR #30):
`NewClient` now builds a fresh `*http.Client` (Transport/Jar/Timeout + noFollow)
instead of value-copying the caller's; the no-follow test asserts Transport/Timeout
preservation and a distinct pointer.

**Update** — Gated the X/Twitter client's 3xx create-outcome ambiguity on a
mutating method (LFXV2-2642, PR #31), matching the same fix applied to the meta
client (PR #30, Cursor review) and the reddit client. `createOutcomeAmbiguous`
had treated every 3xx as UNCONFIRMED regardless of method; now a 3xx is ambiguous
only on a mutating method (a GET redirect is not a create), while 5xx and
transport errors stay ambiguous regardless of method. Added `isMutatingMethod`
and GET/POST/DELETE test cases. All three clients (reddit/meta/twitter) now share
an identical method-gated contract.

## 2026-07-18

**Creation** — Added the `internal/platform/googleads` Go package (GA-1 scaffold,
LFXV2-2636): a Google Ads REST client (not gRPC) with OAuth2 refresh-token auth
(single-flight leader/follower, secret-safe errors), a request layer (no-follow
redirects, bounded reads, pre-send/ambiguous/definite classification, 429 retry
gated on an explicit idempotent flag since GAQL search is POST-but-read-only), and
cursor-paginated GAQL search with page/row caps. customer_id validated digits-only.
GAQL gotcha documented: v23 replaced campaign.start_date/end_date with
campaign.start_date_time/end_date_time. Concept doc + code index updated. Campaign
creation (:mutate), metrics/keywords/audience, and keyword actions follow in
GA-2..GA-5.

**Update** — Routed the project-nested campaign API through the gateway and gave it
real authz (PR #28, LFXV2-2558). The chart previously routed only `/campaigns`, so
the actual contract paths (`/projects/{projectId}/…`) were unreachable. httproute
now uses a `RegularExpression` match selecting this service's project-nested
subpaths (`connection-*`, `briefs`, `jobs`, `{provider}/metrics`,
`google-ads/keywords|audience`, `hubspot`), leaving `project-service`'s `/projects/`
routes untouched. ruleset replaces the `/campaigns` `deny_all` placeholders with a
single `project-api` rule gating every routed family on the project
`campaign_manager` relation (`openfga_check` scoped to `project:{projectId}`, D2),
with `oidc` + `anonymous_authenticator` paired (openfga_check is what rejects the
anonymous subject) and an `allow_all` fallback when OpenFGA is disabled (local dev).
A separate `campaigns-placeholder` rule keeps the still-routed `/campaigns` /
`/_campaigns/*` prefixes fail-closed (`deny_all`), preserving the chart↔route parity
invariant (every heimdall-routed path has a matching rule). deployment readiness
`failureThreshold` relaxed 1→3 for CloudNativePG cold start. Concepts updated:
`httproute`, `ruleset`.
**Update** — Also strengthened the no-follow regression tests (meta + twitter):
they injected a nil-`CheckRedirect` client, which couldn't prove the override is
UNCONDITIONAL (a "fill only nil callbacks" impl would pass). Now they inject a
caller client carrying a SENTINEL `CheckRedirect` and assert the client the code
uses returns `http.ErrUseLastResponse` despite it, while the caller's original
still returns the sentinel (shallow copy, not mutation). (PR #30 review by Copilot.)

**Update** — Typed the X/Twitter Ads client's errors and added outcome
classification (LFXV2-2642). doRequest previously returned a bare fmt.Errorf for
every non-2xx AND echoed the response body into the error string (which can carry
signed URLs / destination secrets and gets persisted into Steps). Added a typed
`apiError` (status/method/path + X's machine-readable error codes, NO body),
`transportError` (ambiguous), `isPreSendDialError`, and `createOutcomeAmbiguous`
(a 5xx apiError or a transportError → UNCONFIRMED regardless of method; a 3xx →
UNCONFIRMED only on a mutating method, since a GET redirect is not a create; a
definite 4xx or a pre-send error → not ambiguous). `isDuplicatePromotedTweetErr`
now matches the typed error code
(DUPLICATE_PROMOTABLE_ENTITY, gated to a 4xx) instead of the no-longer-surfaced
body. Brings X to parity with the reddit/meta/googleads clients. Concept doc updated.

**Update** — Extended the X/Twitter create-outcome classification to the 2xx
edge (LFXV2-2642, PR #31 review by Copilot): a promoted_tweets POST returning a
2xx with no `data.id` was warning "add it manually" — but a 2xx means the POST
succeeded and X MAY have created the association, so a manual re-add risks the
duplicate the classifier prevents. Now that case is surfaced as UNCONFIRMED
(verify before retrying), same wording as the ambiguous-error branch;
`TestPromotedTweetMissingIDWarns` updated to assert the distinction.

**Update** — Gated the X/Twitter duplicate classification to a 4xx (LFXV2-2642,
PR #31 review): `isDuplicatePromotedTweetErr` matched `DUPLICATE_PROMOTABLE_ENTITY`
on any status and ran before `createOutcomeAmbiguous`, so a mutating 3xx/5xx
carrying that code was reported as a known duplicate instead of UNCONFIRMED (the
create may have committed on a 5xx). Now requires a definite 4xx; 3xx/5xx falls
through to ambiguous. Also reworded an UNCONFIRMED warning from "reached X" to
"may have reached X" (a transportError is only plausibly sent), and corrected the
`createOutcomeAmbiguous` log description (status/type-based + caller-scoped, NOT
"any GET failure → clean").

**Update** — Closed a no-body-leak regression in that same X/Twitter `apiError`
(LFXV2-2642, PR #31 review by Copilot): `Error()` was rendering the retained
`ErrorCodes` from the untrusted response body, re-opening the leak channel into
persisted Steps (an untrusted body can place secrets even inside `errors[].code`).
Now `Error()` renders method/path/status only; codes are kept solely for
`hasErrorCode` classification, and `parseErrorCodes` drops over-long values and
caps the count. Mirrors the reddit client's Body-for-classification-only pattern.

**Update** — Disabled HTTP redirect following on the Meta and X/Twitter Ads
clients (LFXV2-2641), closing a duplicate-create gap: both built their
`*http.Client` (and accepted `WithHTTPClient` clients) with no `CheckRedirect`, so
the stdlib could follow a 3xx on a mutating POST after the create was committed and
muddy outcome classification (for X, a followed redirect also resends an OAuth-1.0a
request signed for the original URL). Added a shared `noFollow`
(`http.ErrUseLastResponse`) policy set on the default client and enforced
unconditionally after options via a shallow copy (so a caller's client isn't
mutated) — matching the reddit/linkedin/googleads clients. Regression tests added.
**Update** — Reddit no-follow enforcement now builds a fresh `*http.Client` for a
`WithHTTPClient`-supplied client instead of value-copying it (LFXV2-2641).
`NewClient` did `hc := *c.httpClient; hc.CheckRedirect = noFollow`. The rebuild
carries over only the caller's documented exported fields (Transport, Jar, Timeout)
and sets `CheckRedirect: noFollow`, so it depends on the type's public API rather
than the struct's internal shape (layout-independent) and won't silently carry any
future unexported field. NOTE: this is NOT a race fix — on the repo's Go target
`http.Client` is just those four exported fields with no internal synchronization
state, so the old value copy was also correct (`go vet` copylocks does not flag
it). It's a defensive/clarity change. Strengthened the no-follow test to assert
Jar preservation (in addition to Transport/Timeout) and the caller-not-mutated
guarantee. Scope: reddit only — reddit is the sole client on main enforcing
no-follow on a caller-supplied client (merged via PR #27). The separately-proposed
PRs #30 (meta) and #31 (twitter), still open against main, ADD no-follow to those
clients and construct the client the same way.

## 2026-07-15

**Update** — Hardened the Reddit Ads client's ambiguous-outcome classification
(PR #27): `isPreSendDialError` now proves pre-send ONLY for DNS resolution and
connect-time dial failures (ECONNREFUSED/EHOSTUNREACH/ENETUNREACH). NO TLS error
is treated as pre-send, matching the merged Meta client — a TLS error is not a
reliable pre-send proof for an arbitrary caller-supplied transport (renegotiation,
or a wrapping RoundTripper surfacing a cert/record error while reading a response
after forwarding the POST), so both `*tls.CertificateVerificationError` and
`tls.RecordHeaderError` flow to the UNCONFIRMED path — the safe classification.
Redirect following is still force-disabled on every client used, including one
supplied via `WithHTTPClient` (`CheckRedirect` overridden to
`http.ErrUseLastResponse` UNCONDITIONALLY on a shallow copy, so the caller's
client is not mutated), which keeps 3xx handling well-defined. A 3xx on a MUTATING
request is classified UNCONFIRMED (it reached a responder and may have committed
before redirecting); a 3xx on a GET is not a create. A context error surfaced
from an IN-FLIGHT `Do` stays UNCONFIRMED (the per-attempt ctx wraps the whole
round trip, so it can fire after the POST reached Reddit) — but a cancellation
returned while waiting for token refresh is a proven pre-POST failure
(`refreshToken` returns `ctx.Err()` directly) and remains non-ambiguous.
5xx/mid-flight transport failures also stay UNCONFIRMED. Reworded the
manual-fallback UTM step to SET/REPLACE the utm_* params (matching
`buildRedditUTMURL`'s `url.Values.Set`), keeping all other query params and
dropping a trailing path slash.

## 2026-07-13

**Creation** — Added OKF concept doc for internal/platform/meta (Meta Ads Graph
API client) with `tags`/`timestamp` frontmatter (queryable fields per OKF v0.1
§4.1), listed in the code index.

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/reddit concept doc (queryable fields per OKF v0.1 §4.1).

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/linkedin concept doc (queryable fields per OKF v0.1 §4.1).

## 2026-07-10

**Update** — Addressed Copilot review on the X/Twitter Ads client (PR #19):
create calls now send params as URL query parameters (not a JSON body) per the
X Ads v12 contract, use `entity_status=PAUSED`, and line items carry the
required `start_time`/`end_time` with `bid_strategy` (not `bid_type`); dates are
strictly parsed to reject impossible calendar values; name lookups propagate
errors instead of masking them as not-found. Added the
`internal/platform/twitter` code concept and index entry.

**Update** — Mount connection routes in the HTTP server (LFXV2-2556): the
`cmd/campaign-service` concept now notes that every container-wired service
must also be mounted in `server.go`, or its routes 404 despite compiling.

**Creation** — Added the `internal/platform/reddit` concept doc for the new
Reddit Ads API v3 client (OAuth2 token refresh + Campaign -> Ad Group -> Ad
creation) and listed it in the code index.
**Update** — Hardened claim-based dispatch: resolve the dispatcher and reuse an
already-completed campaign BEFORE claiming (so a no-dispatcher platform never
leaves a permanent pending claim), release the pending claim if dispatch fails
before the upstream campaign is created, and bound concurrent provider calls with
a process-wide semaphore (previously the per-job errgroup limit let N concurrent
jobs each get maxParallelDispatch slots). Shutdown cancels in-flight runs on
drain timeout.

**Update** — Reworked LFXV2-2665 single-flight from a held-connection advisory
lock to an atomic claim row (INSERT ON CONFLICT DO NOTHING of a `pending`
campaign), removing the pool-exhaustion/blocking hazards of holding a connection
across the HTTP dispatch. The pending row is also the recovery signal for an
upstream-create-then-crash. Recovery scan uses a staleness cutoff so a rolling
deploy can't fail a job the old replica is still dispatching.

**Update** — Durable campaign dispatch (LFXV2-2665): per-platform single-flight
via an atomic claim row (ClaimCampaignDispatch — INSERT ON CONFLICT DO NOTHING of
a 'pending' campaign; see the later hardening entries above for the final shape,
which superseded an initial advisory-lock attempt), so concurrent
create-campaigns can't double-create upstream; the orchestrator drains in-flight
runs on graceful shutdown before the pool closes; and startup fails-forward jobs
left non-terminal by a restart. Added CampaignRepository.ClaimCampaignDispatch /
DeleteDispatchClaim and JobRepository.FailStuckJobs.

**Update** — PR #11 review round 3: validate brief_id/campaign_id/job_id path
params as UUIDs (400 instead of a PostgreSQL cast 500); make brief approval
version-gated via If-Match (rejects approving stale content, 412/428); type the
job-poll result (PlatformResult array, replacing Any); and stop applying
debug.LogPayloads to the connection/brief/health endpoints so DEBUG can't leak
BearerTokens or plaintext provider credentials into logs (debug.HTTP header/status
logging is retained). Reconciled api-catalog (PlatformResult; CampaignCreateResult
marked as the future richer shape).

**Update** — Brief + campaign API and async orchestrator (LFXV2-2626):
updated `design`, `internal/service`, and `internal/container` concepts for
the Project → Brief → Campaigns hierarchy, async job dispatch, and idempotent
per-platform creation. Behavior hardened per review: brief content replace
resets status to `draft` and persists `event_slug`; duplicate platform sets are
rejected; dispatch reuses an existing upstream campaign instead of re-creating;
brief responses carry `event_details`/`copy`/`keywords`/`targeting`; the
`(project_id, event_slug)` archived-aware partial unique index moved to a new
migration `000003` (never edit an applied migration in place); `platforms` is
enum-constrained and every brief method declares `BadRequest` (JWTAuth can 400).

**Creation** — Added OKF concept doc for internal/platform/linkedin (LinkedIn
Marketing API client), listed in the code index.

**Update** — Dropped the Goa CLI path allowlist; twitter-api-secret FP is
fingerprint-only in `.gitleaksignore`. Clarified `.grype.yaml` rationale
(Engine fixes exist; Go module path not yet upgradeable via migrate/dktest).

**Update** — Absorbed PR #18 grype fixes into the MegaLinter secrets work:
added `.grype.yaml` (ignore five transitive test-only `docker/docker`
CVEs) and `REPOSITORY_GRYPE_ARGUMENTS` in `.mega-linter.yml`. Kept the
narrower gitleaks allowlists from PR #24 (not #18's broad `^gen/`).

**Update** — Documented local MegaLinter/Docker workflow and tightened
`.gitleaks.toml` allowlists (narrow Goa CLI path + `.gitleaksignore`
fingerprint for twitter-api-secret false positive; sample AES key limited
to docs + `values.local.example.yaml`). Added architecture concept
`megalinter-secrets.md`.

## 2026-07-09

**Update** — Wired `CREDENTIAL_ENCRYPTION_KEY` into the Helm chart and local docs (required whenever a DB URL is configured so `/readyz` can start). Documented a non-production local sample key.

**Update** — Documented PostgreSQL readiness on `/readyz` (LFXV2-2559): updated service/config/container/constants concepts, added `internal/infrastructure/postgres` concept, noted PG* secret injection on Deployment, and added the `002-db-conn-check` feature-spec subtree.

**Creation** — initial OKF knowledge bundle generated from existing docs, Helm charts, Go packages, and speckit specs.
