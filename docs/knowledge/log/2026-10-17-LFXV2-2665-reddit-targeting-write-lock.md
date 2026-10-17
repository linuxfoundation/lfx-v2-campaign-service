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
class `RKTW`), the `ClaimCampaignVersion` mechanism, polled every 100 ms for up to 10 s with no
connection held while waiting — so replicas serialize; without it the dispatcher's in-process lock
serializes one replica. A write still locked out at the deadline is
`ErrKeywordTargetingWriteInProgress`, mapped to 409 beside the stale-revision refusal ("nothing was
removed/added; read again and resend"), with nothing sent. The CONCURRENCY note in
`internal/platform/reddit/keyword_targeting.go`, the design text and `docs/api-catalog.md` now
state the window that remains: an edit made outside LFX between the read and the PATCH, which
Reddit's lack of a conditional write leaves open. Concepts updated:
[Keyword Targeting on Reddit and X](../architecture/keyword-targeting-reddit-x.md),
[internal/dispatch](../code/internal-dispatch.md),
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md),
[internal/platform/reddit](../code/internal-platform-reddit.md),
[internal/service](../code/internal-service.md) and
[internal/container](../code/internal-container.md).
