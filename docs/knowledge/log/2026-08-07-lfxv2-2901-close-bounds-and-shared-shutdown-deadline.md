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

## Second review pass — the shared deadline was still extendable

The absolute-instant fix above made every straggler share ONE expiry. It did not make that
expiry monotonic, and Copilot's follow-up found the hole: `StopCooldownsForShutdown`
published it with a plain `Store`, so the LAST caller won regardless of when its deadline
expires. The doc comment on that function says it is safe to call more than once — so a
second call carrying a longer timeout hands every woken straggler a deadline past the point
the FIRST call's wait returns. The first caller reports the cooldowns drained while a
release is still holding a connection, and `pgxpool.Close` blocks on it outside
`ContainerCloseTimeout`. The overrun the shared deadline exists to prevent, reintroduced one
layer down from where it was fixed.

`publishCooldownReleaseDeadline` keeps the EARLIEST published deadline via a CAS loop. CAS
rather than a mutex because `shutdownReleaseBound` runs on the woken goroutines' path and
must not contend with them. Earliest, not latest, because it is the only direction safe for
both callers at once: the shorter budget still expires before either wait does, and an
expired context makes `releaseCampaignLock` destroy the connection — which is what frees the
pool slot, so out-of-budget is the fast answer rather than the lossy one.

The zero case is the part worth stating explicitly: an unpublished deadline reads as `0`,
which is numerically EARLIER than any real instant. Treating it as a competitor would pin
the bound at the epoch permanently and make every release run on a dead context, shutdown or
not. The CAS therefore treats `0` as "nothing published" rather than as the earliest
deadline, and a subtest covers exactly that. Revert-verified: restoring the plain `Store`
fails the extend subtest and only that one.

**The `Close` budget formula was written down with a term missing.**
`docs/knowledge/code/internal-container.md` listed five of the six terms in
`ContainerCloseTimeout` and omitted `sweeperStopTimeout`, which the constant and
`TestShutdownBudgetComposes` both include. The `init()` panic message enumerated only three.
A budget doc that under-counts is worse than none: the next person adding a `Close` timeout
compares against the doc's sum, concludes there is headroom that is already spent, and the
test that would have caught it is the one they did not know to read. Both now list all six
in the order the constant declares them.

Two log fragments from the 2026-08-06 pass carried no ticket in their slugs
(`toggle-concurrency-test-fake-fidelity`, `toggle-fake-cooldown-release-deadlock`). Renamed
with `lfxv2-2901` per the log-naming rule; nothing referenced the old paths.
