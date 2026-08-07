# 2026-08-07 — Bounding the lock connection's Close, and one shared shutdown deadline

**Update** — Closed the suppressed Copilot findings on PR #78
(`internal/infrastructure/postgres/campaign_repo.go`, `campaign_lock_test.go`, and
`internal/service/brief_test.go`).

**`Close`'s context is not decoration.** Four sites destroyed a possibly-lock-bearing
connection with `context.WithoutCancel(ctx)` and nothing else. pgx uses that context to
bound how long `Close` waits for the session to shut down gracefully, and the pool slot is
NOT returned until `Close` returns — so stripping the cancellation stripped the deadline
with it and left the wait unbounded. That is backwards for exactly these call sites: every
one of them is reached BECAUSE something already failed or was cancelled, so the caller's
context is routinely dead. A failure path became the one path that could pin a request
goroutine and a pool slot indefinitely, and could push `pgxpool.Close` past
`ContainerCloseTimeout` during shutdown. All four now go through `closeLockConn`, which
adds `claimRollbackTimeout` the same way the neighbouring unlock already did. (A fifth
site already used `releaseCtx` and needed no change.)

**A relative budget is not a shutdown budget.** `StopCooldownsForShutdown(timeout)` stored
the timeout as a DURATION, and each straggler that woke afterwards derived a fresh
full-length allowance from it — N stragglers, N × timeout of wall-clock, from a call whose
whole purpose is to cap the wait. It now stores an absolute instant, stamped where the wait
starts, so every straggler shares one expiry no matter when it is scheduled.
`shutdownReleaseBound` returns `time.Until` that instant and deliberately returns a
non-positive remainder AS IS: an already-expired context is the FASTER answer on the
out-of-budget path, because it destroys the connection, and destroying the connection is
what actually frees the pool slot.

The three exact-equality assertions that covered the old behaviour were the reason it
looked correct. They are now `assertWithinShutdownBudget` — `got <= budget`, not
`got == budget` — and that is not a tolerance for slow CI: **the range assertion IS the
property under test.** Only `<=` distinguishes a shared deadline from a per-goroutine
allowance. A new subtest makes the distinction directly, with two stragglers separated by
`budget/5` and an assertion that the later one gets strictly less.

`resetCooldownState` replaced `cooldownWG` without joining it. Assigning a fresh
`sync.WaitGroup` underneath a still-running release goroutine is WaitGroup misuse, and the
kind that surfaces as a flake in an unrelated test rather than where it was caused. It now
DRAINS (`StopCooldownsForShutdown`) before it replaces.

**Two guarantees had no binding test, both for the same reason: the fakes released the
lock immediately.** `toggleCampaignRepo.ReleaseCampaignLockAfterCooldown` unlocks at once —
necessary to keep the sequential tests from deadlocking, but it means a toggle that
(wrongly) took the ordinary inline release would behave identically and every test would
still pass. `cooldownToggleRepo` holds the lock until the test says so, which turns "was
the cooldown release used?" into something observable; the new test then shows a second
toggle parked on the claim, reading the same still-unbumped version, unable to call the
platform a second time on an already-ambiguous change until the cooldown ends — and
proceeding, not failing, once it does. The second new test is the cross-endpoint half of
this ticket: the existing concurrency test races two toggles, but the reason
`UpdateCampaign` claims at all is a race with the OTHER writer. It now blocks while a
toggle is inside its platform call and ends with 412 against the toggle's bumped version,
rendezvous-driven rather than sleep-driven. Both revert-verified in isolation.

The remaining findings in the same reviews were already satisfied on this branch and
needed no change: `ClaimCampaignVersion`'s and `ReleaseCampaignLock`'s doc comments in
`internal/domain/brief_port.go`, the claim-does-not-bump comments in `internal/service`
and its tests, and
`docs/knowledge/log/2026-08-06-lfxv2-2901-advisory-lock-is-not-ownership.md`, which
already carries the corrected account of `ReplaceCampaign` beginning on the claimant's own
connection. One was live: the POOL COST paragraph still said
`service.ToggleCampaignStatus` is the only caller. `service.UpdateCampaign` claims too —
and the two cost very different amounts, which is the part worth writing down: the toggle
holds the claim ACROSS the platform call (where the 45s and the cooldown come from), while
the update performs no I/O between claim and persist. What bounds the concurrent claim
count is that both are one operation per campaign per request. Nothing structural bounds
it, which is the same tracked design change as durable ownership.
