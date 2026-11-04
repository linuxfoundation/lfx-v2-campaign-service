# 2026-11-04 — asset-reservation tests: a wait that cannot flake under -race

**Fix** — test-only; production behaviour is unchanged.

`TestDispatch_LinkedInInvalidStoredImageReachesTheJobResult` failed once in CI (PR #472) with
the dispatch memory refusal ("creative assets (710 bytes) exceed the memory concurrently
available for dispatch") instead of the stored-image refusal it asserts. Its reserver was
`NewAssetReserver(10<<20, time.Millisecond)`. `AssetReserver.reserve` wraps the caller's
context in `context.WithTimeout(wait)` and then calls `semaphore.Weighted.Acquire`, which checks
`ctx.Done()` before it looks at capacity and refuses a done context even when the whole budget
is free. A goroutine stalled for 1 ms between those two calls therefore turns an uncontended
grant into a capacity refusal. Reproduced locally with the CI message: 5 failures in 6,000 runs
under -race with the CPU oversubscribed.

The fix covers every test of this kind. `internal/dispatch/asset_admission_test.go` adds
`testAssetReserveWait` (one second) for reservers whose reservations the test expects to be
granted, and
`testAssetContentionWait` (20 ms) for refusals on a held budget, plus `holdWholeAssetBudget`,
which takes the hold on the semaphore directly so that the hold is not itself a grant racing a
short timer. The two Meta contention tests that refuse on the reserver's own wait keep the
short wait. The two that also assert grants get the long wait, with the refusal bounded by the
caller's context.

`membudget.Budget` is not affected: it admits from free capacity under its lock before it
starts a timer.
