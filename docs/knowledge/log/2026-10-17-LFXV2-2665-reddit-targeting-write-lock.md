# 2026-10-17 — Reddit keyword-targeting writes serialized per ad group

**Fix** — Reddit's two whole-targeting writes, `RemoveKeywordTargeting` and
`AddNegativeKeywords`, each read the ad group's targeting, compared the caller's revision and
PATCHed the whole object back. Two concurrent LFX requests on one ad group (a removal and a
negative add, possibly on different replicas) could both pass the revision check before either
PATCH landed, and the later PATCH wrote back stale targeting — a lost update that could restore a
removed positive keyword and widen delivery. Both writes now hold a per-ad-group lock
(`domain.KeywordTargetingWriteLock`, scope `reddit/{account}/ad_groups/{ad group}`) from the fresh
read through the compare, the PATCH and the confirming re-read. The container binds
`postgres.KeywordTargetingWriteLock` — a session advisory lock (`pg_try_advisory_lock`, two-int
class `RKTW`), the `ClaimCampaignVersion` mechanism, polled every 100 ms for up to
`domain.KeywordTargetingLockWait` (10 s, shared with the fallback) with no connection held while
waiting, and every attempt's pool checkout cut off at that deadline — so replicas serialize;
without it the dispatcher's in-process lock serializes one replica. A write still locked out at
the deadline (or unable to check out a connection by then) is
`ErrKeywordTargetingWriteInProgress`, mapped to 409 beside the stale-revision refusal ("nothing was
removed/added; read again and resend"), with nothing sent; any other lock failure is
`ErrKeywordTargetingLockUnavailable` (503, nothing sent). The holder occupies one pooled connection
for its Reddit calls (why not a dedicated session is in the postgres concept). Neither refusal is
recorded as an upstream Reddit call, and the lock wait is taken out of the recorded upstream
latency (`Orchestrator.recordLockedUpstream`). Release follows the write's outcome (PR #371
review): a write whose PATCH was sent and is UNCONFIRMED — including a negative add answered per
item with an UNCONFIRMED keyword and a nil error — keeps the lock for
`domain.KeywordTargetingUnconfirmedCooldown` (15 s), detached from the request and released at
shutdown through the campaign claim's cooldown machinery (`releaseAfterSharedCooldown`), so a
late-landing PATCH cannot immediately overwrite the next LFX write from its stale snapshot. A
finite cooldown narrows that window; it cannot guarantee Reddit has finished the write. The CONCURRENCY note in
`internal/platform/reddit/keyword_targeting.go`, the design text and `docs/api-catalog.md` now
state the window that remains: an edit made outside LFX between the read and the PATCH, which
Reddit's lack of a conditional write leaves open. Concepts updated:
[Keyword Targeting on Reddit and X](../architecture/keyword-targeting-reddit-x.md),
[internal/dispatch](../code/internal-dispatch.md),
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md),
[internal/platform/reddit](../code/internal-platform-reddit.md),
[internal/service](../code/internal-service.md) and
[internal/container](../code/internal-container.md).

When an unlock fails, the connection close reuses the unlock's own bounded context instead of starting a fresh timeout, so the unlock and the close share one bound and fit `StopCooldownsForShutdown`'s allowance together, as `releaseCampaignLock` does.
