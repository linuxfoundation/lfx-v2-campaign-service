# 2026-08-18 — LFXV2-3222: retention tests that agreed with themselves

**Fix** — Four review items on the `campaign_jobs` retention change, each a case of an
assertion that looked like a guarantee and was not one. One reported item was not a defect
and is recorded here as verified rather than changed.

**A comment claimed a guarantee the code does not provide.** The `jobRetention` field said
`SetJobRetention` "refuses to install a value that would delete more than the default does".
It does not: every positive duration is installed, and a one-hour window deletes far more
than the 180-day default. The method's own doc comment already said so correctly — operators
legitimately want shorter windows — so the field comment was the only thing that was wrong.
The comment was corrected rather than the code: rejecting short windows would break a
supported operator setting, and the guard that actually matters (non-positive means "unset",
never "retain nothing") was already right. This is the same class as the migration comment
fixed earlier on this branch — an assertion of a property nothing enforces.

**A wiring test bypassed the function it was named for.** `TestLoadCampaignJobRetentionFromEnv`
asserted `parseRetention(os.Getenv(...))`, re-implementing the line under test instead of
running it. Deleting the `CampaignJobRetention` assignment out of `LoadConfig` left it green:
the operator's window would reach nothing and the feature would be inert with a passing wiring
test. It now calls `LoadConfig` and reads the field off the returned value. Doing that needs
`flag.CommandLine` and `os.Args` swapped for the duration — `LoadConfig` registers flags
globally and calls `flag.Parse`, so a second call panics with "flag redefined" — which is why
the test had been written the easy way in the first place.

**A test agreed with its own copy of the status vocabulary.** The prune's allow-list decides
which rows get DELETED, and those rows are the audit trail of real ad spend. The test pinning
it against `model.JobStatus.Terminal()` iterated a hand-written list of the five statuses, so
a status added to the model but forgotten there would go unclassified while the test kept
passing "in both directions" over four statuses out of five. The vocabulary is now declared
once as `model.AllJobStatuses`, and both the repository test and the live EXPLAIN test derive
from it. `TestJobStatus_Terminal` pins the new list in turn, failing if a status is added
without being deliberately classified — otherwise the shared list would just be a new
hand-copy nobody checks.

**A live test proved the index existed, not that the prune uses it.** Its own comment said it
asserted "through EXPLAIN, because only the planner can say whether the index is actually
usable", while the body queried `pg_indexes` for a row with the right name. It now runs
`EXPLAIN` against the prune's actual inner SELECT. Two details make that meaningful. It sets
`enable_seqscan = off` inside a transaction — not to force a pass, but because on a test-sized
table a sequential scan is genuinely cheapest, so an EXPLAIN of the default plan reports
`Seq Scan` with the index perfectly correct; disabling it asks the question that holds at any
table size, "can the planner use this index for this predicate". And it rejects a residual
`Filter:`, which is what distinguishes an index that is merely used from one whose partial
predicate implies the prune's status clause.

**Not a defect: the sweeper shutdown test.** `TestRetentionSweeperStopsOnShutdown` was
reported as shutting down before the first prune fires, and it does — the interval is an hour.
But it never claimed to test pruning: it pins the sweeper's lifetime to `sweeperCtx`, and a
mutation replacing that select arm with a channel that never fires makes it fail (and hang for
its full five-second timeout) rather than pass. Pruning-per-tick is covered separately by
`TestRetentionSweeperPrunesOncePerTick`, added earlier on this branch, which polls until the
prune happens. Both tests assert real properties; neither is redundant, and nothing was
changed.

Each fix was mutation-tested with a change that COMPILES, since a build break proves nothing.
Dropping the `LoadConfig` assignment, dropping the retention index, narrowing its predicate to
omit `'failed'`, making it non-partial, adding a sixth status to the vocabulary, and adding
`'running'` to the delete allow-list were each caught, by the specific test claimed to cover
them. That last one — the mutation that WIDENS what gets deleted — is caught twice: by the
derived unit test and, against live PostgreSQL, by the test proving an aged `running` row
survives a prune. The EXPLAIN test's negative control was confirmed the same way: with the
index dropped, the planner falls back to `Seq Scan` even with seqscan disabled, because the
only other candidate is partial over the complementary statuses.

Verified against live PostgreSQL 16, not by reading: the retention live tests execute rather
than skip, and the index mutations above were applied to a real database and rolled back.
