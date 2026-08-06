# 2026-08-06 — LFXV2-2901: fix vacuous concurrency test (Cursor Bugbot)

**Finding** — Cursor Bugbot flagged
`TestBriefService_ToggleCampaignStatus_ConcurrentTogglesSerialize`
(`internal/service/brief_test.go`) as non-binding: it made its two
`ToggleCampaignStatus` calls sequentially, one after the other, so it could
assert the same final-state outcome (one success, one 412) even against an
implementation that never actually serialized concurrent writers. The root
cause was in the fake, not just the test: `toggleCampaignRepo.ClaimCampaignVersion`
did an unprotected in-memory version check with no locking, unlike the real
`CampaignRepo.ClaimCampaignVersion` (`internal/infrastructure/postgres/campaign_repo.go`),
which holds a per-campaign Postgres advisory lock on a dedicated connection —
a concurrent claim on the same campaign *blocks* until the lock is released,
it does not race an in-memory read.

**Fix** — `toggleCampaignRepo` now models that blocking behavior:
- `claimMu sync.Mutex` is the fake's analog of the advisory lock: acquired in
  `ClaimCampaignVersion`, held until `ReleaseCampaignLock` unlocks it, so a
  second concurrent claim genuinely blocks rather than racing a version check.
- `dataMu sync.Mutex` guards `got`/`replaced` against `GetCampaign` (called
  outside `claimMu`, before any claim is held) racing the claim holder's
  `ReplaceCampaign`.
- `ClaimCampaignVersion` re-checks `expectedVersion` only *after* acquiring
  `claimMu`, matching the real code's actual ordering — the version bump
  itself still happens in `ReplaceCampaign`, not in the claim.

`TestBriefService_ToggleCampaignStatus_ConcurrentTogglesSerialize` was
rewritten to launch two real goroutines released simultaneously (via a
closed `start` channel) instead of two sequential calls, and to assert on an
atomic in-flight counter (`concurrentStubToggler.maxInFlight`) that the
platform never sees more than one call in flight at once, in addition to the
existing "exactly one success, one 412" and "persisted version is the
claimed version, not either caller's stale `IfMatch`" assertions.

**Verification** — per the standing checklist item on vacuous tests: reverted
the underlying LFXV2-2901 fix by temporarily patching
`BriefService.ToggleCampaignStatus` to bypass `ClaimCampaignVersion` and
reuse the already-checked in-memory version instead (simulating the
pre-#2901 defect), then re-ran the new test. It failed immediately — with
`brief.go` bypassing the real claim, `ReleaseCampaignLock` is called without
a matching `ClaimCampaignVersion` lock acquisition, so the test crashes with
`fatal error: sync: unlock of unlocked mutex` rather than passing. This
confirms the new test is binding on the real serialization mechanism, not
just on the fake's final state. `brief.go` was restored afterward; only
`internal/service/brief_test.go` carries a diff for this fix.
