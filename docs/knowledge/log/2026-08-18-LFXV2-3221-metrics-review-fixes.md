# 2026-08-18 — LFXV2-3221 metrics review fixes: buckets, stuck-job accuracy, assertions

**Fix** — Review of the `/metrics` PR found five minor issues, three of which defeated
the metrics' own stated purpose: a histogram whose buckets could not resolve the
latencies this service produces, a stuck-job counter that went quiet for exactly the
stuck jobs it existed to expose, and upstream instrumentation nothing asserted. Each is
a case of the metric still being *served* while no longer *answering anything* — the
failure mode observability code has when it is wrong, since nothing 500s.

**Histogram buckets must come from the call budgets, not the default.**
`campaign_upstream_call_duration_seconds` is fed `time.Since(start).Seconds()` but the
`MeterProvider` set no view, so the instrument inherited the OTel SDK's default explicit
boundaries — `{0, 5, 10, 25, 50, 75, 100, 250, ...}`, which are chosen for
**millisecond** values. Fed seconds, the first positive bucket is `(0, 5]`, so every
healthy call landed in one bucket and the p50/p95/p99 the histogram exists to produce
could not separate 50ms from 4s. The boundaries are now derived from what this service
actually enforces: `toggleCallTimeout` is 45s, the three read paths are 20s, and the
Microsoft client caps a request at 30s, so the ladder runs `{.01, .025, .05, .1, .25,
.5, 1, 2.5, 5, 10, 20, 30, 45}` with edges sitting **on** the timeout ceilings, making
"this call timed out" a bucket boundary rather than something smeared across one.

The view selects by instrument name, and a view whose selector misses does **not**
error — it silently restores the defaults. The name is therefore a shared constant
(`upstreamDurationInstrument`) used by both the registration and the selector, and the
test asserts sub-second boundaries in the **scrape output** rather than reading the
boundaries variable, so selector drift fails rather than passing vacuously.

**A terminal transition that never persisted blinds the alert it feeds.**
`campaign_job_transitions_total` exists so a stuck job appears as the GAP between
`running` and the terminal statuses. `RecordJobTransition` ran unconditionally after
`UpdateJobStatus`, whose error is only logged — so when the finalizing write failed, the
row stayed `running` in the database while the counter recorded a terminal, closing the
gap for precisely the rows the alert hunts. The alert went quiet at the moment it should
have fired. Both finalize paths now route through one `terminalize` helper that records
only after a successful write; such rows are terminalized later by the recovery sweeper
(`FailStuckJobs`), and the gap stays open until they are.

The asymmetry with the RUNNING transition is deliberate and is now written down at both
sites: RUNNING is recorded on **attempt** (dispatch proceeds regardless, so gating it
would under-count during a database blip), while the terminal one is recorded only on
**success** (its whole meaning is that the job really did terminalize).

**The marshal-failure arm is unreachable from a normal dispatch — so test it directly.**
Guarding only the common path left the marshal-failure arm's guard unasserted: a
mutation reverting it to unconditional recording compiled and the entire suite stayed
green. `platformResult` is all plain types, so `json.Marshal` cannot fail on it and no
dispatch-level test can reach that arm. Factoring both arms through `terminalize` gave
the two paths a single testable point, and a table-driven test now covers write-succeeds
and write-fails for each. This is the general lesson: when a branch cannot be reached
through the public path, that is the argument for extracting it, not for leaving it
untested.

**A metric nothing asserts is a metric that can silently stop recording.** The test
double captured `RecordUpstreamCall` into a slice no test ever read, so the operation
constants, the error→outcome mapping, and the after-the-guards ordering could all have
broken or been dropped with every test still passing. All five instrumented paths are
now driven end to end across success and error arms, asserting the bounded operation
token, the outcome, and the platform. A companion test pins the **negative**: a
pre-platform refusal (no dispatcher, capability unsupported, campaign not provisioned)
records nothing, since those return in nanoseconds and would both inflate the error rate
with local refusals and drag every quantile toward zero.

**A derived Helm key cannot be authoritative if the user map is merged after it.** The
deployment template rendered `prometheus.io/port` from `service.port` and then merged
`.Values.podAnnotations` verbatim. A user setting that key in `podAnnotations` produced
a **duplicate YAML key**, and last-key-wins meant their value silently pointed the
scraper at a port the container was not listening on — the exact drift the derived key
was introduced to prevent, while README and `values.yaml` both asserted it "cannot
drift". The key is now `omit`ted from the user map before rendering, so the invariant is
enforced rather than merely documented, and a chart test renders the hostile override to
prove it. Every other annotation still passes through.

**`operation` is bounded by shape, not by a closed enum.** Unlike `platform` and
`outcome`, `operation` reached the label verbatim; the bound was five compile-time
constants at the call sites. A closed map was deliberately **not** the fix: the operation
vocabulary is per-platform and grows as call sites are instrumented, so a map would
silently collapse each newly instrumented call to `unknown` — invisible, because the
metric keeps being served. `safeOperation` is a shape guard instead: short lower-snake
tokens pass through (a correct new constant is not penalised), while anything carrying
digits, uppercase, whitespace, punctuation or excess length — the shape of a *derived*
string — degrades to the bounded token. This bounds the blast radius of a future mistake
without punishing correct growth.

**Two owned resources that were not owned.** `PoolStats.NewConnsCount` was collected
from the pool but exported by no instrument, so the struct implied a series `/metrics`
did not serve; it is now `campaign_db_pool_new_connections_total`, which is what
separates "the pool is busy" from "the pool is churning". `Registry.Shutdown` was
implemented and documented but never called from `Container.Close`, unlike every other
owned resource; it now runs **last**, after every source that records into it has
stopped, and is non-fatal so a flush failure cannot mask a real drain error.

**A recovered panic must not rewrite a result that already landed.** After
`dispatchPlatform` returned, its result was stored in `results[i]` and the outcome was
recorded. A panic in that recording call was caught by the dispatcher's `recover`, which
unconditionally reassigned `results[i]` from `res` — never assigned the success — turning
a campaign that really was created upstream into a synthetic failure, and inviting a
reconcile or retry that could **double-create a paid campaign**. Reachability was
effectively nil (the shipped recorder is a plain counter `Add`), but the code comment
claimed the separate `done` variable prevented this, and it did not. A `dispatched` flag
set after the result is stored now makes the recover arm skip the overwrite: losing one
metric is strictly cheaper than reporting a live paid campaign as failed. `rerr` is
cleared on **both** arms — recovering but returning the panic as the group error would
cancel every sibling platform, which is the crash the recovery exists to prevent.

**Mechanical, but worth recording.** Inserting the metrics block between a doc comment
and its function detached **two** of them: `SetIndexer` (flagged in review) and
`NewOrchestrator` (not flagged, found while fixing the first). godoc attaches such a
comment to whatever follows, so the block had adopted the `const` declaration. Both are
reattached. When an insertion strands one doc comment, check its neighbours.
