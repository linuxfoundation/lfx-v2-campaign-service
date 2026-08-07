# 2026-08-05 — LFXV2-2901: bound the cooldown by the shutdown budget; no stale release

**Fix** — `errcheck` lint failure and two Bots findings on PR #78's
`unconfirmedLockCooldown` release (both Cursor and Copilot independently flagged
the same defect class).
The unlock-failure paths added in
[`2026-08-05-lfxv2-2901-lock-connection-reuse-and-unconfirmed-cooldown.md`](2026-08-05-lfxv2-2901-lock-connection-reuse-and-unconfirmed-cooldown.md)
(`ClaimCampaignVersion`'s
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
