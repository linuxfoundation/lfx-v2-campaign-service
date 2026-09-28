# 2026-09-28 — LFXV2-2665: a cancelled caller was charged to the provider

**Fix** — round 16 of local review. One metrics defect, two doc claims that had gone stale under
their own code, and two test handoffs that `-race` was entitled to flag.

## A context cancelled before the probe started became an upstream error sample

`googleads`, `microsoft` and `reddit` each begin token acquisition with an entry check: if the
caller's context is already done, return without building, dialling or sending anything. That
check returned a bare `ctx.Err()`.

Bare is the problem. `ProbeInconclusive` claimed it by its `true` default — correctly; nothing was
learned about the connection. `ProbeNotSent` did not claim it, because its default is `false` and
it recognised only DNS failures and dial refusals. `probeReachedThePlatform` then found no local
sentinel to name, and `recordUpstream` wrote a
`campaign_upstream_call_duration_seconds{outcome="error"}` sample — a near-zero-latency error, on
the one series that is supposed to mean the platform, for a request no platform ever saw. The rule
it broke is `internal-service.md`'s own: upstream calls are timed only after the pre-platform
guards pass, precisely so local refusals do not distort that series.

Each package now declares its own `errTokenContextAlreadyDone` and wraps `ctx.Err()` in it at that
entry check, and each `ProbeNotSent` reads its own marker.

Three decisions inside that are worth keeping:

**Only the entry check is marked.** `refreshToken`'s waiter `select` returns `ctx.Err()` as well,
and marking it would have been one line. It would also have been false: a coalesced refresh may
already be on the wire when a waiter's context expires, so "nothing was sent" is a claim that
check cannot make. An unmarked context error keeps the `false` default and still records a
sample, which is the cheap direction.

**Microsoft did not reuse `errRequestNotSent`.** It already has a pre-send marker, and the
temptation was obvious. Its doc scopes it to the REST path ALONE — a fact the round-9 fix
depended on — so widening it to the token leg would have made a written contract false to save a
declaration. The two markers stay distinct because they claim different things.

**The marker wraps rather than replaces.** `fmt.Errorf("%w: %w", marker, err)` keeps
`errors.Is(err, context.Canceled)` and `context.DeadlineExceeded` answering for every caller that
already branches on cancellation.

The regression test asserts the fact, not the claim: an httptest server counts every request it
receives, an already-cancelled probe must leave that count at zero, and only then is the predicate
asked. All three fail against the pre-fix predicate on exactly that assertion.

## `ConnectionProber` documented the opposite of `ok`'s contract

The interface said an inconclusive outcome "must not be rendered as a failed test". The service
renders it as `OK: false`, and `design/connection.go` says so explicitly, because `ok` reports a
CONJUNCTION and an incomplete check establishes neither half as a whole.

Both sentences were defending the same thing — an unreachable platform must never be reported as a
rejected credential — and the interface comment reached for the nearest words rather than the
contract's. It is the primary guidance for six implementations, so the nearest words were the
expensive ones: a reader following them restores `OK: true` for a check that proved nothing. It
now names what the outcome must not be rendered as (a confirmed connection failure or credential
rejection), states that it still answers `OK: false` with an inconclusive advisory, and points at
the design file that owns the contract.

This is the second doc-vs-code divergence found in two rounds on the same subject, both in the
same direction: the prose describing `ok` drifted toward "the credential is fine" while the code
held the conjunction.

## `ProbeNotSent`'s one-line summary had been narrowed back

`internal/dispatch/probe.go`'s vocabulary block summarised it as "nothing left this process, so no
platform was reached", and `orchestrator.go`'s sentinel list said its error "proves the request
never left the process".

Round 11 settled that this is wrong for the three two-leg platforms: the subject is the request
whose failure DECIDED the probe, and a token refresh may have reached the platform and succeeded
before the account read failed to dial. The detailed docs were corrected then; the summaries were
not, and a summary is what a maintainer reads first. Left alone, the obvious "tightening" is to
narrow the predicate to match its own one-liner, which silently restores the defect.

Both now say what the detailed docs say, and both name the multi-leg case rather than leaving it
to be discovered.

## Two httptest handoffs without a happens-before edge

`docs/reviews/knowledge-base/test-hygiene.md`'s `httptest-handler-state-needs-synchronized-handoff`,
twice: `googleads`'s `reachServer` recorded `searches`, `searchPath` and `searchQuery` on handler
goroutines and asserted on them from the test goroutine, and `microsoft`'s request-boundary tests
counted with a bare `int`.

`reachServer` gained a mutex and an `observed()` snapshot, so the three fields are read as one
consistent set rather than three separately-racing ones. The Microsoft counter became an
`atomic.Int32`. The second matters more than it looks: the assertion there is that the count stays
ZERO, and an unsynchronized zero proves nothing at all — the test would have kept passing for the
wrong reason on the day the validation regressed.
