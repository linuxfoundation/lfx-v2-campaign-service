# 2026-09-28 — a racy, flaky googleads cancellation test failed `make test` at random

**Fix** — surfaced by round 28's reviewer as a validation note, not a finding. Pre-existing, in a
file LFXV2-2665 never touched. Test-only change.

`TestUpdateCampaignStatus_CancelDuringBackoffIsUnconfirmed` counted requests in a plain `int`
written by `net/http`'s handler goroutine and read by the test goroutine. `make test` runs
`-race` and CI runs `make test`, so the package could fail on any PR, for a reason no PR caused.

It carried a second defect the race hid. Cancellation was a bare `time.Sleep(50ms)` in a
goroutine, betting that the token exchange plus the first API request would finish inside it.
Roughly one run in ten locally they did not: cancel landed before anything was sent, `hits` stayed
`0`, and the test failed its own fixture guard — `fixture did not send a request, so there is no
ambiguity to classify`. Reproduced at `-count=25`: two outright failures plus the race.

Both are now structural rather than timed. `hits` is an `atomic.Int64`, and the handler signals a
buffered channel after writing the `429`; the canceller waits on that signal, sleeps 20ms so the
response finishes being read, then cancels. "Cancelled AFTER a request was sent" — the premise the
test exists to exercise — is true by construction instead of by scheduling luck, and the cancel
still lands inside the 2s `Retry-After` backoff with three orders of magnitude of margin.
`-race -count=40` is clean.

Worth naming: the flake and the race were the same line of thinking, and the race detector found
the one nobody had chased. A test that fails its own fixture precondition one run in ten reads as
infrastructure noise, which is how it survived — the failure message blames the fixture, so nobody
reads it as a bug in the test.
