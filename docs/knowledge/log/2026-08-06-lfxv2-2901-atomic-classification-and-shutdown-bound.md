# 2026-08-06 — LFXV2-2901: lock-held 404/412, shutdown-bounded release, honest fakes

**Update** — Closed the remaining suppressed Copilot findings on PR #78.

1. **404 vs 412 is now classified while the lock is still held.**
   `CampaignRepo.ClaimCampaignVersion` used to release the advisory lock and its
   connection, then call `GetCampaign` on a fresh pooled connection to decide whether a
   no-rows claim meant "gone" or "stale version". A delete committing in that gap turned a
   stale-version caller's contractual 412 into a 404. The probe
   (`SELECT EXISTS (...)`) now runs on the SAME locked connection, before the unlock, so
   the row's existence is pinned to the instant the version comparison was made.

2. **The cooldown lock release is bounded by the shutdown budget, not by its own.**
   `ReleaseCampaignLock` keeps its ordinary `lockReleaseTimeout` (5s), but an internal
   `releaseCampaignLock(ctx, token, timeout)` variant lets the shutdown path pass Close's
   much smaller budget. `StopCooldownsForShutdown` publishes that budget
   (`cooldownReleaseBoundNanos`) BEFORE closing the wake channel, and the cooldown
   goroutine picks the bound by wake reason. This matters because `pgxpool.Close` blocks
   until every checked-out connection is returned: without the hand-off, `Close` returned
   after its own wait elapsed while a stalled unlock ran on for 5s, and the pool close then
   sat through the difference OUTSIDE `ContainerCloseTimeout`. A failed unlock DESTROYS the
   connection, which is what actually frees the pool slot (Postgres drops a dead session's
   advisory locks itself).

3. **`ClaimCampaignVersion` is documented as a lock, not a version bump.** The stale
   "`UPDATE ... SET version=version+1 WHERE version=$expected`" description survived in
   `internal/domain/brief_port.go`, both `VALIDATION MUST HAPPEN BEFORE CLAIMING` comments
   in `internal/service/brief.go`, and `docs/knowledge/code/internal-service.md`. The claim
   leaves the version UNCHANGED; the increment lives in `ReplaceCampaign` so it co-commits
   with the outbox event.

4. **Two fakes were lying about that lifecycle.**
   `campaignEditRepo.ClaimCampaignVersion` bumped the version and its `ReplaceCampaign` did
   not — the exact inverse of production. `TestBriefService_UpdateCampaign_ValidationBeforeClaim`
   therefore only detected the fake's invented bump. The bump moved to the fake's
   `ReplaceCampaign`, and the test now observes the claim directly via a `claims` counter.
   `TestBriefService_ToggleCampaignStatus_ConcurrentTogglesSerialize` relied on a 20ms
   sleep to make both goroutines read the same pre-claim version; a `readBarrier`
   rendezvous in `toggleCampaignRepo.GetCampaign` now guarantees it. The sleep stays only
   to widen the `maxInFlight` probe's window; no guarantee rests on it.

**Verification** — each fix reverted and re-run:
- Moving validation after the claim in `UpdateCampaign` →
  `ClaimCampaignVersion was called 1 times before validation rejected the request, want 0`.
- Dropping the `cooldownReleaseBoundNanos.Store` before the channel close →
  `would unlock with a 5s budget, want 250ms`.
- Making the toggle fake's claim non-blocking →
  `platform saw 2 toggle calls in flight at once` on every one of 3 runs, deterministically,
  where before the barrier the same break could pass on a lucky interleaving.
