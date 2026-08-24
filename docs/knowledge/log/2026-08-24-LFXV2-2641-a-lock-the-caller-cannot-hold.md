# 2026-08-24 — a lock the caller cannot hold

**Fix** — `keywordActionServers` (internal/dispatch tests) returned a `*bool` whose write, on
the httptest server goroutine, was guarded by a `sync.Mutex` local to the helper. Ten assertion
sites read `*reached` — and one reset it — with no lock, because there was no lock they COULD
take: the mutex was a local variable the helper never handed back. The synchronization was real
on one side and absent on the other, which is the same as absent.

## Why this is worth an entry when the suite was green

Reverting the fix does NOT make `-race` fire on this suite, and that is the interesting part.
Every current call site reads the flag only after a SYNCHRONOUS dispatcher call has returned,
and that return supplies the happens-before edge. The tests are correct today by the shape of
their callers, not by anything the flag guarantees.

That distinction matters because the edge is invisible at the assertion. Nothing in
`if *reached {` says "this is safe because the call above was synchronous", so nothing preserves
it: a handler that outlives the call — a retry, a background token refresh, a cancelled request
still in flight — reintroduces the race silently. The isolated pattern (write under a local
mutex, unlocked read, request still in flight) DOES fire under `-race`, reproducibly if
intermittently — 1 in 5 runs.

The failure mode is the one that matters most for these particular assertions: they are
"the platform was NOT contacted" guards. A non-deterministic read of a flag that should be
false mostly reads false, so the assertion PASSES while proving nothing. A test that can be
right by luck is worse than a missing test, because it is counted.

## The fix

`atomic.Bool`, returned by pointer. Callers use `Load()` / `Store(false)`. The synchronization
moves to the VALUE, which is the only thing both goroutines actually share — so a caller cannot
reintroduce the asymmetry by forgetting a lock it never had access to.

The general form: a mutex that guards a value must be reachable by everyone who touches the
value. If a helper keeps the lock and hands out the data, it has not synchronized anything; it
has just moved the race out of its own file.
