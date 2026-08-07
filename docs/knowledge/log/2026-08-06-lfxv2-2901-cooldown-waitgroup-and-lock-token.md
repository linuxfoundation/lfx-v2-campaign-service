# 2026-08-06 — LFXV2-2901: cooldown WaitGroup ordering, and a token-scoped release

**Fix** — PR #78 review round: three remaining findings on the campaign
advisory-lock lifecycle.
1. **`cooldownWG` happens-before violation.** `ReleaseCampaignLockAfterCooldown`
   could call `cooldownWG.Add(1)` concurrently with `StopCooldownsForShutdown`'s
   `Wait()`, violating `sync.WaitGroup`'s contract that an `Add(1)` starting from
   zero must happen before the matching `Wait`. Fixed with a `cooldownMu` mutex
   guarding a `cooldownStopped` flag: `ReleaseCampaignLockAfterCooldown` checks
   the flag under the lock and either releases synchronously (post-shutdown) or
   calls `Add(1)` before unlocking and spawning the goroutine;
   `StopCooldownsForShutdown` sets the flag under the same lock before closing
   `cooldownShutdown` and waiting. Added
   `campaign_lock_test.go` (new file — this package had no lock-lifecycle
   coverage) with a 50-goroutine race test, verified binding by reverting the
   guard and observing `go test -race` fail 3/5 runs.
2. **Stranded advisory lock on acquire error.** `ClaimCampaignVersion`'s
   `pg_advisory_lock` call can report an error to the client (e.g. the request
   context is cancelled mid-call) while Postgres granted the lock server-side. A
   bare `conn.Release()` on that error path returned the connection to the pool
   with the lock still held on its session — session advisory locks aren't
   released by returning the connection. Fixed by destroying the connection
   (`conn.Conn().Close`) before releasing, matching the pattern already used for
   the guarded-read failure path in the same function.
3. **Stale release via re-loaded map reference.** `ReleaseCampaignLock` called
   `activeCampaignLocks.Load(campaignID)` immediately before `CompareAndDelete`,
   which trivially "succeeds" against whatever is currently in the map — not
   necessarily the lock this caller claimed. If the original session died during
   the UNCONFIRMED cooldown and Postgres auto-dropped its lock, a new claimant
   could `Store` a successor lock under the same `campaignID`; the delayed
   release would then release the successor's live lock. Fixed by introducing
   `domain.CampaignLockToken` — an opaque handle returned by
   `ClaimCampaignVersion` and threaded through every release call — so
   `CompareAndDelete` always compares against the caller's own claimed
   `*campaignLock`, never a freshly re-loaded value. The token lives in `domain`
   (not `postgres`) to avoid an import cycle between the `CampaignWriter`
   interface and its concrete lock type; the handle field is opaque (`any`) and
   only `postgres` type-asserts it back.
