# Test hygiene

Two patterns from the test suite. Both matter more here than they would elsewhere,
because `.gitleaks.toml` allowlists **every `*_test.go` file from every rule** and
because the tests are what stand between a refactor and live, spending ads.

---

## pin-the-injected-clock-in-date-bearing-client-tests

**Severity:** `high`.

**Detect:** A `*_test.go` under `internal/platform/**` or `internal/dispatch/**`
that passes a `StartDate`/`EndDate` (or JSON `startDate`/`endDate`) as a
**near-future literal**, where the client under test is constructed **without** an
injected clock. The option name differs per package — check for the right one:

| Package | Field | Option |
|---|---|---|
| `googleads`, `linkedin` | `now` | `WithClock` |
| `meta` | `timeNow` | `WithClock` |
| `reddit` | `now` | `WithNowFunc` |
| `hubspot` | `now` | `withClock` (unexported) |
| `twitter` | `nonceFn`, `timeFn` | none — no `WithClock` exists |

The two remedies used in-repo are to pin the clock to a fixed instant, or to use
far-future `2098`/`2099` date literals so schedule validation passes regardless of
the wall clock.

**Why it matters:** create paths reject a start date in the past relative to the
injected clock. A test that hardcodes a near date and does not pin the clock
passes today and goes red on a calendar date, with a failure that points nowhere
near any real defect.

**Evidence:**

- [`r3562864162`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562864162)
  (PR #20) predicted it precisely: "These tests hardcode StartDate/EndDate as
  2026-08-01/2026-08-31, but CreateCampaign now rejects start dates in the past
  (relative to c.timeNow()). Once the real date passes 2026-08-01, a large portion
  of this file will start failing regardless of code correctness." Fixed in
  `2bad40a`.
- Developer fixing commits on merged PRs: `2df2ba254` ("pin the clock in
  date-based tests") and `d3d3f51ba` ("clock-pin remaining date tests") on
  **#20**; `fcd7f27` ("pin test clock") on **#21**; `70b3057` ("inject a fixed
  clock in the reddit tests to remove time-dependence") on **#36**.

**Status on main:** the helpers exist per package and are **not** shared —
`fixedClock()` at `internal/platform/googleads/client_test.go:36` and again at
`internal/platform/linkedin/client_test.go:40`, and `fixedMetaClock()` at
`internal/platform/meta/client_test.go:26`. In `internal/dispatch/*_test.go` the
clock is an inline literal pinned to 2098 or 2099 rather than a named helper.

**Time-sensitive note recorded 2026-07-29:** `internal/platform/meta/client_test.go`
carries 61 `2026-08-01` fixture literals against a `fixedMetaClock()` pinned to
`2026-07-15T12:00:00Z`. The pin is what keeps the largest test file in the repo
green; a patch that removes or bypasses it breaks the file outright.

**Not a finding when:** the test's whole point is real time — a timeout or
backoff test — or the dates are already far-future `2098`/`2099` literals. Do not
demand a `WithClock` option in `twitter`, which has none; there the injected
fields are `nonceFn` and `timeFn`.

---

## httptest-handler-state-needs-synchronized-handoff

**Severity:** `should-fix`, or `high` when the unsynchronised value is what an
assertion depends on.

**Detect:** In a test using `httptest.NewServer`, either form:

1. A variable written inside the handler function and read by the test goroutine
   with no happens-before edge — no mutex, no atomic, no channel. Captured
   counters, `gotAuth`/`gotBody`-style request captures, and captured maps are the
   recurring shapes.
2. `t.Fatal`, `t.Fatalf` or `FailNow` called **inside** the handler. `FailNow`
   must run on the test goroutine only; from a handler it does not stop the test
   and can leave the server blocked while a deferred `Close()` runs.

Also flag a fixed `time.Sleep` used as the synchronisation between the handler and
the test, and an unbounded channel receive that hangs the suite when the code path
regresses before reaching the handler.

**Why it matters:** `httptest.Server` runs each handler in its own goroutine.
`make test` runs `go test -race` (`Makefile:95`), so the unsynchronised handoff is
a detected failure, not a theoretical one — and the flake surfaces as an unrelated
test failing on a loaded runner.

**Evidence:**

- [`r3563472288`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563472288)
  (PR #20): "These variables are written by `httptest.Server` handler goroutines
  and read by the test goroutine without synchronization." Fixed in `aeb6fff`.
- [`r3563472323`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563472323)
  (PR #20) is the `FailNow` half: "`decodeBody` runs inside `httptest.Server`
  handler goroutines, but `t.Fatalf` calls `FailNow`, which the `testing` package
  requires to run only from t…". Fixed in `aeb6fff`.
- [`r3573756297`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/19#discussion_r3573756297)
  (PR #19) — the counter shape, fixed in `d085658` ("race-safe call counters").
- [`r3554869081`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/21#discussion_r3554869081)
  (PR #21) — "`gotAuth`, `gotUA`, and `gotBody` are written inside the httptest
  handler goroutine and read from the test goroutine"; fixed in `731a0cc`.
- [`r3608593613`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/29#discussion_r3608593613)
  (PR #29) — the deferred-`Close` deadlock variant, fixed in `9fed68c`
  ("fix single-flight test hang-on-failure").
- 12 comments matching this pattern's core shape — unsynchronised handoff, or
  `FailNow` from a handler goroutine — across 4 vendors (`twitter` 1, `meta` 5,
  `reddit` 5, `googleads` 1) and 4 merged PRs (#19, #20, #21, #29), with
  `9d4d9c4` a further fix on #20. Widening the filter to this entry's full
  scope, including the `time.Sleep`-as-synchronisation and unbounded-receive
  variants above, gives **16** over the same 4 PRs and the same 4 vendors; the
  4 extra are all `reddit` on #21. Both counts are reproducible — see
  [Corrections on record](README.md#corrections-on-record) for the probes and
  for the superseded figure.

**Status on main:** `internal/platform/meta` and `internal/platform/reddit` are
the clean reference implementations. Sites remain in `twitter`, `linkedin`,
`googleads`, `hubspot`, `microsoft` and `internal/dispatch/twitter_test.go`. On
PR #19 the developer fixed the four counters in the inline anchor and left the
rest. Treat those as unadopted sites, not as licence to add more.

**Not a finding when:** the handler writes only to values it alone owns, or the
handoff already goes through a mutex, an atomic or a channel. A `t.Errorf` inside
a handler is acceptable — it records a failure without `FailNow`'s
goroutine restriction. Do not raise this against pre-existing sites the patch does
not touch.
