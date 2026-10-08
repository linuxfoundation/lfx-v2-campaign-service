# 2026-10-06 — Pin the upsert's slot lock, and the adopt contract

**Verification** — review of #260. `TestLiveUpsertWaitsForTheSlotLock` holds the per-slot advisory
lock with nothing inserted and asserts that `UpsertCampaign` on the empty slot cannot finish until
the lock is released, so the third writer the lock serializes (the upsert's INSERT arm) is now
covered; removing its `lockCampaignSlot` call fails the test. The `CampaignWriter.AdoptCampaign`
contract in `internal/domain/brief_port.go` now describes the locked any-live-row check as
load-bearing, with `ON CONFLICT DO NOTHING` kept as the final same-version guard. See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md).
