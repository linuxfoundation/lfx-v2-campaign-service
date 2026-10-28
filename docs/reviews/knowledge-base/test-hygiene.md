# Test hygiene

Three patterns from the test suite. All matter more here than they would
elsewhere, because `.gitleaks.toml` allowlists **every `*_test.go` file from every
rule** and because the tests are what stand between a refactor and live, spending
ads. The first two are about a test that goes red for the wrong reason; the third
is about one that cannot go red at all.

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
`fixedClock()` in both `internal/platform/googleads/client_test.go` and
`internal/platform/linkedin/client_test.go`, and `fixedMetaClock()` in
`internal/platform/meta/client_test.go`. Each is named rather than given a line,
for the reason this file's sibling entry records: a line number is a claim that
goes stale on the next edit above it, and all three of these had already drifted.
In `internal/dispatch/*_test.go` the clock is an inline literal pinned to 2098 or
2099 rather than a named helper.

**Time-sensitive note recorded 2026-07-29, re-counted 2026-10-27:**
`internal/platform/meta/client_test.go` carries **80** `2026-08-01` fixture
literals — it was 61 when this note was first written — against a
`fixedMetaClock()` pinned to `2026-07-15T12:00:00Z`. The pin is what keeps the
largest test file in the repo green; a patch that removes or bypasses it breaks
the file outright. The count is recorded with its date because it only ever
grows, and a reader who finds a different number has learned the file is still
accumulating fixtures, not that the note is wrong.

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
`make test` runs `go test -race` (the Makefile's `test` target), so the unsynchronised handoff is
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

---

## a-test-that-cannot-fail-against-the-code-it-names

**Severity:** `high`. Four of the verified instances below sit on a credential
validator, a tenant-scoped query, a client cache the create path was supposed to
use, and a guard that must refuse before a paid HubSpot create — each one a test
that was green while the thing it was named for was absent.

**Detect:** A new or modified `*_test.go` whose assertions cannot distinguish the
production change in the same range from that change being undone. Deleted is the
commonest undoing but not the only one, and the entry's own instances include the
other two: PR #160's call site reverted to direct construction, and PR #205's guard
moved below `CloneEmail`. So read "undone" as any bounded edit that still compiles —
deleting the line, replacing it with what stood there before, or reordering it past
a neighbouring statement. Four shapes, each decidable from the diff plus a bounded
look at the target:

1. **The test calls the helper, the diff adds the call site.** The test invokes
   `validateX`, `cachedXClient`, `resolveXClient` or similar **directly**, while
   the production hunk in the range is the *call to it* from a handler,
   dispatcher or constructor. Nothing in the range drives the exported entry
   point, so removing that one line leaves the package green.
2. **The test inspects an artifact instead of running the code.** `Contains` /
   `require.Contains` over a package-level SQL constant, a generated file's text,
   a built request body, or a constant that mirrors a production constant — and
   the function the test is named for is never executed. A substring proves the
   clause is in the text; it cannot prove the bind arguments land on the columns
   they name, that an exclusion removes rows, or that the value has not drifted
   from the one it mirrors.
3. **The fixture absorbs the branch.** The fixture supplies a value that makes
   the asserted outcome over-determined — the expected error is also what the
   fake returns on its own (an empty store answering `ErrNotFound`, so an
   assertion that only requires *an* error passes with the validator deleted —
   `mapErr` renders that one as a 404 and the validator's as a 400, which an
   error-shape-blind assertion never separates), or the case name says
   *omitted field* / *zero status* while the fixture sets the field.
4. **The assertions stop at the return value and never reach the side effect.**
   The fixture is fine and the branch does run; what is missing is any assertion
   on what the code *did* before it returned. The test checks the returned error
   or sentinel and discards the recorder, fake or output argument that would show
   whether a write, publish or upstream create happened first. A returned refusal
   is equally produced by a guard that refuses before the side effect and by one
   that refuses after it, so the assertion cannot see the guard move — which is
   the ordering regression, not the refusal. This is the shape that survives
   *reordering* rather than deletion, so shape 1's deletion test will not find it.

The confirming question, and the one the fixes below all answer in their reply:
**which single statement of the production diff could be deleted, reverted to its
predecessor, or moved past its neighbour — the package still compiling — with this
test still passing?** If that statement is the one the change adds, it is a match.
Keep the edit to **one logical statement** and to something that compiles: one
line, or one `if` block, loop or call expression moved or removed as a unit — a
guard is a three-line `if` and still counts as one. What does not count is a
mutation needing several *coordinated* edits in different places; that is not
evidence this test is non-binding.

**Why it matters:** this repo has no mutation testing and `make lint` runs bare
`golangci-lint` with no config (see *Why review-only*), so nothing mechanical
notices. A non-binding test is worse than a missing one — it is counted as
coverage of exactly the path it does not cover, and the gap is found by the next
refactor rather than by the suite. On the paid-create paths the failure mode is
a partial credential trio persisted behind a 400, or a double-create.

**Evidence:**

- [`r3818569514`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/148#discussion_r3818569514)
  (PR #148), shape 1: "The all-or-none rule is tested only by calling
  `validateLinkedInRefreshCredentials` directly. Deleting this handler call leaves
  every current test green while `CreateLinkedinAds` again persists a partial
  refresh trio and silently degrades it to bearer-only." Fixed in `54f4f61e`
  ("test(connection): bind the validator call sites, not just the rules"), which
  found **7 of 9 call sites were survivors, not 2**, and whose own reply records
  shape 3 nearly absorbing the fix: `fakeRepo.SetCredential` returns `ErrNotFound`
  on a missing row, so against an empty store "that error would otherwise stand in
  for the validator's" — the set-credential test seeds the row first for exactly
  that reason. The two are not the same status (`mapErr` maps `domain.ErrNotFound`
  to 404 and the validator's refusal to 400, `internal/service/connection_handler.go`),
  so the stand-in is caught by any assertion that pins the status; what it defeats is
  an assertion that only requires an error. The commit adds
  `createCalls`/`setCredentialCalls` so the assertion is *no write reached the
  repository*, not merely a refusal.
- [`r3835001215`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/153#discussion_r3835001215)
  (PR #153), shape 2: "This query is the tenant boundary for both new read
  endpoints, but its tests only assert SQL substrings." Fixed in `6c81de47`, which
  added a live test asserting the exact **ID set** rather than a count — "a count
  of two is also what the wrong two rows produce, which is the cross-tenant
  failure itself" — and killed five compiling mutations.
- [`r3817365666`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/160#discussion_r3817365666)
  (PR #160), shape 1: "None of the new Microsoft cache tests exercises this
  dispatch wiring; all call `resolveMicrosoftClient`, so reverting this line to
  direct construction leaves them green." Fixed in `ffcaf80f` ("test(dispatch):
  bind the client-cache tests to what they assert") — "the cache was well tested
  and simply unused by the create path it exists for".
- [`r3963631870`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/205#discussion_r3963631870)
  (PR #205), shape 4: "both subtests discard `hubspotRec` and only inspect the
  returned sentinel", so moving the guard below `CloneEmail` would have kept them
  green while orphaning a draft. Fixed in `fc275f25`, which left the fixtures
  untouched and added recorder assertions — the fixtures were never the problem,
  which is what separates this from shape 3. The mutation is the guard's
  three-line `if` moved as a unit, which is why the confirming question is bounded
  to one logical statement rather than one line.
- **Recurrence: 17 Copilot comments of this core shape across 16 pull requests**
  since 2026-08-01 — #59, #64, #87, #96, #102, #107, #125, #147, #148, #153,
  #160, #186, #205, #217 (×2), #268, #272. All but #125 are on merged PRs; #125
  is closed-unmerged and counts toward recurrence only, never as a fix. The
  vocabulary is stable enough to grep for: *leaves them green*, *still passes
  if*, *does not exercise*, *only assert*.
- Local review agents raised fourteen further instances on 2026-08-19 across this
  repo and `lfx-self-serve` (see #162 for the list). Those are **not** gate
  evidence — no Copilot thread — and are recorded only because they are why the
  candidate was filed.

**Status on main:** unresolved as a class, not as a site list. The fixes above
bound four specific paths; the shapes recur because each new test is written
beside the rule it checks rather than beside the handler that asks.

**Not a finding when:**

- The split is deliberate **and documented at the test**. The canonical example
  is `TestDeleteCampaign_LocksRowBeforeGuards`
  (`internal/infrastructure/postgres/campaign_repo_test.go`, found by name rather
  than by line — it moves), which asserts on `deleteCampaignLockQuery`'s text
  alone and says so in its own doc comment,
  because the guard *decision* it protects is tested exhaustively as
  `model.CampaignStatusDeletable`. Copilot raised exactly this on PR #64
  ([`r3715909195`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/64#discussion_r3715909195))
  and the developer **declined with reasons**, accepting the critique of the test
  while keeping the split. A comment naming what the test does not cover, and
  where that half is covered instead, is the remedy — not a defect.
- The uncovered call site is pre-existing and the range does not touch it. This
  is a finding against a change, not a licence to audit the suite.
- The test is a compile-time or type-level pin, where there is no behaviour to
  execute.
