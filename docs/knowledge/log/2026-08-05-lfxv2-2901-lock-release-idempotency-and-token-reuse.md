# 2026-08-05 — LFXV2-2901: idempotent lock release, token-scoped ReplaceCampaign connection

**Update** — Addressed two review findings against `internal/infrastructure/postgres/campaign_repo.go`:

1. **`ReleaseCampaignLock` was not idempotent** (Cursor). It unconditionally executed the
   advisory unlock and disposed of `lock.conn` regardless of whether the preceding
   `activeCampaignLocks.CompareAndDelete` succeeded, so a second call with the same token could
   reuse a connection already returned to the pool (and possibly already handed to a different
   caller). `campaignLock` now carries a `released atomic.Bool`, and `ReleaseCampaignLock`
   `CompareAndSwap`s it before touching `conn`/`lockKey` — a repeat call with the same token is
   now a true no-op, matching the method's documented contract.
2. **`ReplaceCampaign` attached to whatever connection was currently stored for the campaign ID**
   (Copilot), via `activeCampaignLocks.Load(c.ID)`. If the original claimant's session had died
   and a successor's `ClaimCampaignVersion` overwrote the map entry, this could silently run the
   write on the successor's connection instead of the caller's own — corrupting the mutual
   exclusion the lock exists to provide. `ReplaceCampaign` now takes the caller's own
   `domain.CampaignLockToken` (already in scope at both call sites, from the preceding
   `ClaimCampaignVersion`) and uses `token.Handle().(*campaignLock).conn` directly, falling back
   to `r.db` only when no lock is held. `CampaignWriter.ReplaceCampaign`'s signature, both call
   sites in `internal/service/brief.go` (`UpdateCampaign`, `ToggleCampaignStatus`), and the three
   test fakes/stubs implementing the interface were updated to match.
