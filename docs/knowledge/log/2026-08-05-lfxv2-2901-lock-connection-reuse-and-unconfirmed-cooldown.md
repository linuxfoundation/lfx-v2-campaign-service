# 2026-08-05 — LFXV2-2901: reuse the claim connection; cool down UNCONFIRMED releases

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
