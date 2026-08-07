# 2026-08-06 — LFXV2-2901: fix Cursor Bugbot deadlock finding

**Update** — `toggleCampaignRepo` (added in the prior fake-fidelity fix)
holds `claimMu` from a successful `ClaimCampaignVersion` until
`ReleaseCampaignLock` unlocks it, but never overrode
`ReleaseCampaignLockAfterCooldown` and kept the embedded
`fakeCampaignRepo`'s no-op. `ToggleCampaignStatus`'s UNCONFIRMED path
(`internal/service/brief.go`) skips the deferred `ReleaseCampaignLock` and
calls `ReleaseCampaignLockAfterCooldown` instead, so `claimMu` was never
unlocked on that path — any later claim against the same
`toggleCampaignRepo` instance would block forever.
`TestBriefService_ToggleCampaignStatus_UnconfirmedIsSurfaced` already
exercises this path; it happened not to deadlock only because it never
claims a second time on the same fake.

**Fix** — Added `toggleCampaignRepo.ReleaseCampaignLockAfterCooldown`,
unlocking `claimMu` immediately (the fake has no real connection to hold
open for the cooldown window, unlike the Postgres-backed implementation).

**Verification** — per the standing checklist item on vacuous tests:
temporarily removed the new override and added a throwaway test that calls
`ToggleCampaignStatus` twice on the same fake (first with an unconfirmed
platform error, then normally). Without the override it hung and `go test
-timeout` reported the goroutine blocked on `claimMu.Lock()` inside
`ClaimCampaignVersion`, confirming the deadlock is real. Restored the fix
and removed the throwaway test; only the permanent override remains.
