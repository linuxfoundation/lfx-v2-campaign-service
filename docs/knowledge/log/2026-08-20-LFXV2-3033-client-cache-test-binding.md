# 2026-08-20 — LFXV2-3033 client-cache review follow-ups: tests that did not bind

Four review findings against the client-cache change. Three were real; one was reported with a
false mechanism but pointed at a real defect a few lines away. Each real one is recorded with the
compiling mutation that proves the fix binds.

## A test probe that dialled Microsoft production on every run

`microsoftProbe` drives `GetCampaignMetrics`, which submits to the REPORTING origin
(`c.reportingBaseURL`). The fixture overrode only `WithBaseURL` (Campaign Management) and
`WithTokenURL`, so the reporting origin kept its production default and every `make test` and CI
run issued real outbound requests to `https://reporting.api.bingads.microsoft.com`.

The tests still PASSED in that state, which is what made it invisible: the probe deliberately
discards its API error, and the OAuth exchange it exists to force had already happened against the
stub. So the cache assertions were resting on a live network failure rather than on the cache, and
the suite would fail closed in a sandbox with no egress. Measured cost: the three Microsoft
client-cache tests took 3.0s, and 0.27s once the origin was stubbed.

`microsoft.Client` splits its API across THREE hosts (Campaign Management, Customer Management,
Reporting) and an un-overridden origin silently falls back to the production default rather than
failing loudly, so all three are now overridden through one `newMicrosoftCacheDispatcher` helper
that the tests share — a per-test override list is exactly what drifts.

Timing is not an assertion, so the regression guard is a dialler:
`TestClientCache_MicrosoftProbeStaysOnTheStub` installs an `http.Client` whose `DialContext`
REFUSES any non-loopback address, making an escaped origin a deterministic failure at the point it
happens rather than a slow green test.

## A mutex that made the -race concurrency test serial

Both cold-key burst tests took the bookkeeping mutex with `mu.Lock(); defer mu.Unlock()` BEFORE
calling the probe, so the `defer` held it for the rest of the goroutine and all 16 callers ran
their probe strictly one at a time. The tests asserted a property they did not exercise: `-race`
cannot observe a data race in code that never runs concurrently.

This was not a theoretical gap. Deleting every `c.mu.Lock()`/`Unlock()` pair from
`reddit.Client.refreshToken` — a compiling change that removes the production-side synchronisation
outright — left `TestClientCache_RedditColdKeyConcurrentBuildsAreCoalesced` PASSING under `-race`.

The lock now covers only the appends to the shared slices; the probe runs outside it. Re-running
the same mutation now fails with `WARNING: DATA RACE` on `refreshToken`'s read/write of the
inflight handle, reported through `redditProbe`.

## Dispatch wiring that nothing asserted

Every client-cache test drove `resolveMicrosoftClient`, the toggle/metrics entry point.
`MicrosoftDispatcher.Dispatch` builds its client on a separate line, and reverting that line to a
direct `microsoft.NewClient(...)` — again a COMPILING change — left the entire suite green. The
cache was thoroughly tested and simply not used by the create path, which is the path it exists
for: a dispatch burst re-minting an OAuth token per campaign.

What binds it is the token COUNT across two dispatches of one unchanged connection: cached
construction mints once, direct construction mints twice. Under the revert the new test now fails
with `token endpoint hit 2 times ... want 1`.

## A concurrency comment that was incomplete — and the stale claim behind it

The review reported that `c.geo.snapshot` and `c.geo.inflight` mutate under `tokenMu`. That is
FALSE: they mutate under `c.geo.mu`, a deliberately separate lock (a token refresh is a small JSON
round trip, a locations refresh is a multi-MiB download; one mutex would let a slow file fetch
stall every token read). No code was changed for the mechanism as reported.

The conclusion was still right for a different reason. `MicrosoftDispatcher.clients` claimed "the
only fields written after construction are the token cache and the in-flight refresh handle, both
exclusively under c.tokenMu" — and the geo snapshot and its in-flight fetch are also written after
construction, under the other lock. The comment now enumerates BOTH locks and says why they are
separate. The identical sentence in this ticket's own earlier log fragment is corrected the same
way.

`googleads.go`'s matching sentence was checked and left alone: that client genuinely has no geo
cache on the receiver, and its only post-construction writes are the token cache and inflight
handle under `tokenMu`.

The related defect is that this ticket FALSIFIED a scope comment it did not update. `geoCacheTTL`
in `geo.go` and the geo section of `internal-platform-microsoft.md` both stated that
`MicrosoftDispatcher` "builds a NEW client per Dispatch call, so this coalesces WITHIN one campaign
create — not across jobs." Caching the client is precisely what made that false: one client, and
therefore one parsed locations map, is now reused ACROSS creates for the same connection until the
credential rotates or the entry is evicted. Both now describe the real scope, including what is
still NOT true — it is not a process-wide cache, since separate connections and post-rotation
clients each re-fetch. This is also what promotes `geo.mu` from defensive to load-bearing:
concurrent creates on one connection genuinely share that cache now.

`docs/knowledge/log/2026-08-19-LFXV2-3279-geo-reuse-reconcile.md` carries the same
now-superseded scope claim. It is left intact: it was accurate when written, and log fragments are
dated historical records owned by their own entry.
