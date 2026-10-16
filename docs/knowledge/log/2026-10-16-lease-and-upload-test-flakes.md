# 2026-10-16 — Stats-job lease admission and upload-admission test flakes

**Fix** — Two flaky tests, both test bugs; no product code changed.

`TestLiveStatsJobLease_NewLockIsNotAdmittedAfterTheRequestEnds` failed under machine load with
"the lock taken for an ended request must stay held and tracked, not orphaned" (13 of 1,200
runs at load ~25). It cancelled the request on a 100ms timer, assuming `Own` would by then be
inside its slow (`pg_sleep(0.3)`) lock statement. But `Own` first opens the lease session, and
that connect stops when the request ends. Under load the connect outlasted the timer, so the
request was refused before any lock was taken. That refusal is correct, but it is not the case
the test asserts. The test now cancels only once the lock statement is in flight. The statement
first takes a transaction-level advisory "gate" lock that a test transaction holds. The test
polls `pg_locks` until a backend waits on the gate (bounded at 10s), cancels the request, and
then rolls back to open the gate. The in-package live lease tests also name their accounts with
the pid, a nanosecond clock and a counter (`liveLeaseSuffix`). A wall-clock string alone could
repeat across parallel test processes, which then saw each other's locks as "held elsewhere".

`TestUploadAdmission_WaitsBrieflyRatherThanShedingInstantly` could hang until `go test`'s
timeout. Its first handler signalled with a non-blocking send (`select` with `default`). If it
ran before the test reached the receive, it skipped blocking, and the test waited forever on a
signal that never came (2 hangs in 4,800 runs across 16 parallel processes). The first handler
now blocks on its own first-call flag and closes a channel. Every wait in the test is bounded.
The test also checks that the second request has not finished while the permit is held, and
that it was not admitted until the first was released.

After the fix there were 0 failures in 2,400 lease runs (12 parallel processes) and 4,800 upload
runs (16 processes) at load ~28, plus 100 runs each under `-race` and with `GOMAXPROCS=1`.
Removing the lease's `heldKeys` tracking, or the request check in `admit`, still fails the
rewritten lease test.
