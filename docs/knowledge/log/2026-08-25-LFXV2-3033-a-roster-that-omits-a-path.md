# 2026-08-25 — LFXV2-3033 a roster that omits one of its own paths

**Fix** — the client-cache roster in `credcache.go` declares itself the per-path source of
truth and then listed `TwitterDispatcher` on "its create AND toggle paths". Its METRICS path
is wired too: `ReadMetrics` calls `resolveTwitterClient` (`twitter.go:488`), which returns
`cachedTwitterClient` — the same call `ToggleStatus` makes at `twitter.go:426`. Both callers
share one entry; only the roster disagreed. `docs/knowledge/code/internal-dispatch.md` carried
the identical omission, so the mirror confirmed the error rather than catching it.

**This is the roster-as-prose failure again.** A list of wired paths written as a sentence is
invisible to every grep that would check it: nothing fails when a path is added and the sentence
is not, and a second copy in the docs makes the claim look corroborated. The fix is only the
words — no behaviour changes — but the words are what the next reader will trust when deciding
whether a path is cached.

**Also qualified an overstated guarantee.** `internal-platform-twitter.md` said "a caller whose
context is cancelled reserves nothing". `pace` does not provide that: cancellation can land
between the final `ctx.Err()` check and the `nextWrite` update, and
`TestPaceCancelAtWaitExpiryReservesNothing` explicitly documents and tolerates that residual
window — measured near 0.5%, against roughly 98% before the fix. The doc now says a cancellation
OBSERVED BEFORE ADMISSION reserves nothing, and names the residual window, so the test and the
prose describe the same guarantee. The test name is stronger than what it asserts; the prose is
where the qualification belongs.
