# 2026-08-05 — Campaign write serialization review rounds (LFXV2-2901)

Consolidated record of PR #78's review rounds on the claim-based campaign-write
serialization. Entries run oldest first.

**Update** — Closed the toggle If-Match TOCTOU race across all campaign writers
(LFXV2-2901). `BriefService.ToggleCampaignStatus` previously guarded optimistic
concurrency with a read-time in-memory `existing.Version != version` check BEFORE
the side-effecting platform call; that only rejects a stale caller, it does not stop
a SECOND caller reading the same version and also proceeding to mutate the
platform. Added `CampaignRepo.ClaimCampaignVersion` (`UPDATE campaigns SET
version=version+1 WHERE ... version=$expected`), an atomic version-gated bump with
no content column, mirroring `ReplaceCampaign`'s existing not-found/precondition-
failed disambiguation (prior art: `ClaimCampaignDispatch`'s `INSERT ... ON CONFLICT
DO NOTHING`). Both `ToggleCampaignStatus` (before the platform call) and
`UpdateCampaign` (before its DB-only write, which has no I/O gap of its own but can
otherwise win the row out from under an in-flight toggle's claim) now claim first,
so the version column serializes every campaign writer through one atomic gate
rather than each writer protecting only itself.

**Fix** — Three correctness issues with the claim-based serialization (PR #78 review).
1. `ClaimCampaignVersion` now uses `UPDATE ... RETURNING` to atomically read the
   post-update row, preventing a separate `GetCampaign` re-fetch from being
   interleaved by concurrent writers and returning stale/unclaimed versions.
2. `UpdateCampaign` validation (status mismatch check) now happens BEFORE
   `ClaimCampaignVersion`, so a rejected request (400) does not bump the version.
   Previously, validation after claiming meant a retry of the same rejected
   request would get 412 PreconditionFailed instead of the original error.
3. Documented a known limitation: `ClaimCampaignVersion` provides serialization
   only for concurrent callers reading the SAME version. If caller A claims and
   then makes a long platform call (e.g., toggle to ad platform), caller B can
   read the newly-bumped version DURING A's call, claim it, and enter its own
   platform call concurrently. A future fix should use durable in-flight
   ownership (e.g., lease token or explicit "in-flight" status) to prevent this
   scenario; until then, it's accepted as a small-window edge case.

**Fix** — Stale `If-Match` on `UpdateCampaign` was misclassified as 400 instead of
412 (Cursor Bugbot finding, PR #78 review).
Item 2 in the entry directly below moved the status-mismatch validation ahead of
`ClaimCampaignVersion` to stop a rejected request from bumping the version. That
introduced a new bug: without checking the `If-Match` version first, the
status-mismatch check validates the client's payload against whatever row is
CURRENTLY in the database, not the version the client actually read. A concurrent
`ToggleCampaignStatus` can flip `existing.Status` between the client's read and this
request, so a stale-but-otherwise-valid update gets compared against the new status
and rejected with 400 ("use the status-toggle endpoint") — the wrong error for what
is actually a stale-ETag conflict.
Fixed by checking `existing.Version != version` immediately after loading `existing`,
before the status-mismatch check, mirroring the pattern already used correctly in
`UpdateAudience` (`internal/service/audience.go`). Added
`TestBriefService_UpdateCampaign_StaleVersionIsPreconditionFailed` to cover a stale
If-Match against a row whose status changed concurrently.

**Fix** — Advisory-lock connection handling in the campaign-write serialization
(PR #78 review, 3 findings + 1 duplicate).
`ReplaceCampaign` always opened a second pooled connection via `r.db.Begin`, while
`ClaimCampaignVersion` held its own dedicated connection with the session advisory
lock for the whole claim window. With `pool_max_conns=1` every successful toggle
blocked until `persistResultTimeout`; with larger pools, enough concurrent claims
could exhaust the pool entirely. Fixed by having `ReplaceCampaign` reuse the
claim-holder's connection (looked up via `activeCampaignLocks`) when one is held for
the campaign being written, instead of always acquiring a fresh one.
Both the claim-error path in `ClaimCampaignVersion` and the normal path in
`ReleaseCampaignLock` unlocked with a possibly-cancelled/plain context and returned
the connection to the pool even when the unlock failed. A session advisory lock is
NOT released just by returning the connection to the pool — it survives for the life
of the underlying Postgres session — so a failed unlock could strand the lock on a
connection later claims would be handed, blocking them permanently. Fixed both paths
to unlock on a detached bounded context and destroy (rather than release) the
connection whenever the unlock itself fails.
Separately: `ToggleCampaignStatus`'s UNCONFIRMED branch released the claim lock
immediately (via the deferred release) while deliberately leaving the row
untouched — but that let a second waiter already blocked on the lock claim the same
still-unbumped version and call the platform again while the first call's outcome
was still unknown. Fixed by skipping the immediate release on that path and
releasing asynchronously after a bounded `unconfirmedLockCooldown` (30s) instead, so
the lock still drops immediately on a crash (it's connection-scoped) but a live
process gives an operator/retry a window before another writer can proceed.

**Fix** — `errcheck` lint failure and two Bots findings on PR #78's
`unconfirmedLockCooldown` release (both Cursor and Copilot independently flagged
the same defect class).
The unlock-failure paths added in the entry below (`ClaimCampaignVersion`'s
claim-error path and `ReleaseCampaignLock`) called `conn.Conn().Close(ctx)`
without checking its error, failing CI lint. Fixed by capturing and
`slog.WarnContext`-logging the close error in both spots.
Two deeper issues in the same cooldown mechanism:
1. **Shutdown-budget overrun.** The UNCONFIRMED-branch release ran as a bare
   `go func() { time.Sleep(unconfirmedLockCooldown); ... }()`, holding a pooled
   connection for up to 30s with no awareness of shutdown. `pgxpool.Close` (called
   from `Container.Close`) blocks until every checked-out connection returns, so
   this could make `Close` overrun the 25s `DefaultShutdownTimeout` and risk a
   SIGKILL mid-drain. Fixed by replacing the raw goroutine with
   `CampaignRepo.ReleaseCampaignLockAfterCooldown`, which races the cooldown
   against a `cooldownShutdown` channel closed by a new
   `postgres.StopCooldownsForShutdown(timeout)`. `Container.Close` now calls
   `StopCooldownsForShutdown(cooldownStopTimeout)` immediately before
   `pool.Close()`, mirroring the existing stuck-claim-sweeper shutdown pattern
   (`sweeperStopTimeout`); `cooldownStopTimeout` (250ms) is a new term in
   `ContainerCloseTimeout`, so the existing budget-sum invariant test and the
   `init()` panic-on-overrun check automatically cover it. Added
   `ReleaseCampaignLockAfterCooldown` to the `CampaignWriter` port.
2. **Stale-release race.** `ReleaseCampaignLock` deleted its map entry
   unconditionally by key (`LoadAndDelete`). If the original session died during
   the 30s cooldown, Postgres auto-drops its session-scoped advisory lock, and a
   later claimant could `Store` a *new* `*campaignLock` under the same
   `campaignID` — the delayed release would then delete-and-release that
   successor's lock and connection out from under it, reopening the exact
   concurrent-write window the lock exists to prevent. Fixed with
   `sync.Map.CompareAndDelete`: the release is now a no-op unless the map still
   holds the exact `*campaignLock` this caller was handed.

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

**Fix** — Cursor Bugbot finding on PR #78's `ReleaseCampaignLock`: the
`CompareAndDelete` failure branch (a newer claimant already overwrote the map
entry) returned early without disposing `lock.conn`, leaking that checked-out
pool connection for the life of the process — a failed compare-and-delete says
nothing about whether THIS token's own connection still needs releasing, since
nothing else references it. Fixed by making the map bookkeeping
(`CompareAndDelete`) and the connection disposal unconditional-vs-conditional
independently: the map delete only removes this session's own entry (as
before), but `lock.conn` is now always unlocked/released below it regardless of
whether that delete matched. The sibling `ClaimCampaignVersion`'s plain
`Store` overwrite the bot also flagged doesn't need a separate fix — every
successful claim already has an eventual `ReleaseCampaignLock(token)` call
(deferred or cooldown-scheduled) that holds the original `*campaignLock` via
the token's closure, independent of the map's current contents, so this one
fix closes both flagged sites.
