// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// insertJobAged inserts a campaign_jobs row in the given status whose updated_at is `age` in
// the past, and returns its id.
//
// updated_at is set EXPLICITLY rather than by waiting: retention windows are measured in days,
// so a test that aged rows by sleeping could not run. The column has a now() default and no
// trigger, so writing it directly is exactly what the passage of time would have produced.
func insertJobAged(ctx context.Context, t *testing.T, pool *pgxpool.Pool, briefID string, status model.JobStatus, age time.Duration) string {
	t.Helper()

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_jobs (brief_id, status, created_at, updated_at)
		VALUES ($1, $2, now() - $3::interval, now() - $3::interval)
		RETURNING id::text`, briefID, string(status), age.String()).Scan(&id)
	if err != nil {
		t.Fatalf("insert %s job aged %s: %v", status, age, err)
	}
	return id
}

// jobExists reports whether a job row is still present.
func jobExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM campaign_jobs WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("count job %s: %v", id, err)
	}
	return n > 0
}

// TestLivePruneTerminalJobsSparesEveryNonTerminalRow is the guard this whole change turns on.
//
// campaign_jobs is the audit trail of real ad spend, and the prune is the only code in the
// service that DELETES from it. The property that makes it safe is not "old rows go away" — it
// is that a queued or running row is never eligible however old it is, because an old
// non-terminal row is a STUCK JOB, which is precisely the record someone needs in order to
// investigate a dispatch that never finished.
//
// Every status in the CHECK constraint is exercised, aged far past the window, in one pass:
// asserting only on 'queued' would leave 'running' to be swept in by a predicate that named
// just the one status.
func TestLivePruneTerminalJobsSparesEveryNonTerminalRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewJobRepo(&postgres.Pool{Pool: pool})

	briefID, _ := insertApprovedBrief(ctx, t, pool)

	// Everything is aged WELL past the window handed to the prune, so age can never be the
	// reason a row survives — only its status can.
	const age = 72 * time.Hour
	const window = time.Hour

	terminal := map[model.JobStatus]string{
		model.JobSucceeded: insertJobAged(ctx, t, pool, briefID, model.JobSucceeded, age),
		model.JobPartial:   insertJobAged(ctx, t, pool, briefID, model.JobPartial, age),
		model.JobFailed:    insertJobAged(ctx, t, pool, briefID, model.JobFailed, age),
	}
	nonTerminal := map[model.JobStatus]string{
		model.JobQueued:  insertJobAged(ctx, t, pool, briefID, model.JobQueued, age),
		model.JobRunning: insertJobAged(ctx, t, pool, briefID, model.JobRunning, age),
	}

	if _, err := repo.PruneTerminalJobs(ctx, window, 0); err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}

	for status, id := range nonTerminal {
		if !jobExists(ctx, t, pool, id) {
			t.Errorf("a %s job aged %s was DELETED: a non-terminal row is a stuck job and is "+
				"the record needed to investigate a dispatch that never finished", status, age)
		}
	}
	for status, id := range terminal {
		if jobExists(ctx, t, pool, id) {
			t.Errorf("a %s job aged %s past the window survived the prune; retention does not bound the table", status, age)
		}
	}
}

// TestLivePruneTerminalJobsRespectsTheWindow pins the age boundary: a terminal job that reached
// its terminal state INSIDE the retention window is still live history and must be kept.
//
// Without this, a prune whose interval arithmetic was wrong (or absent) would pass the
// status-guard test above while deleting everything terminal on its first pass.
func TestLivePruneTerminalJobsRespectsTheWindow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewJobRepo(&postgres.Pool{Pool: pool})

	briefID, _ := insertApprovedBrief(ctx, t, pool)

	const window = 24 * time.Hour
	recent := insertJobAged(ctx, t, pool, briefID, model.JobSucceeded, time.Hour)
	old := insertJobAged(ctx, t, pool, briefID, model.JobSucceeded, 48*time.Hour)

	if _, err := repo.PruneTerminalJobs(ctx, window, 0); err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}

	if !jobExists(ctx, t, pool, recent) {
		t.Error("a terminal job INSIDE the retention window was deleted; the window is not being applied")
	}
	if jobExists(ctx, t, pool, old) {
		t.Error("a terminal job outside the retention window survived")
	}
}

// TestLivePruneTerminalJobsBoundsTheBatch proves the LIMIT is real against PostgreSQL.
//
// The bound is what stops one sweep over a large backlog from holding a long transaction and
// locking campaign_jobs against live dispatch writes. It is asserted by the ROW COUNT, not by
// reading the SQL text: a LIMIT that was dropped, or bound to the wrong placeholder, leaves
// source text that still mentions LIMIT.
//
// It also pins that a backlog DRAINS across passes rather than being abandoned — a bound that
// silently stopped making progress would keep the table growing just as surely as no bound.
func TestLivePruneTerminalJobsBoundsTheBatch(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewJobRepo(&postgres.Pool{Pool: pool})

	briefID, _ := insertApprovedBrief(ctx, t, pool)

	const total = 5
	ids := make([]string, 0, total)
	for range total {
		ids = append(ids, insertJobAged(ctx, t, pool, briefID, model.JobSucceeded, 72*time.Hour))
	}

	// surviving counts how many of THIS test's rows are left. The package migrates one shared
	// schema, so other tests' aged rows are eligible for the same prune and land in its return
	// count — asserting on that count alone would make this test depend on which siblings ran
	// first. Every assertion here is therefore scoped to ids.
	surviving := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM campaign_jobs WHERE id = ANY($1::uuid[])`, ids).Scan(&n); err != nil {
			t.Fatalf("count this test's jobs: %v", err)
		}
		return n
	}

	// A limit well below the eligible count. The pass must delete at most the bound — this is
	// what stops one sweep over a large backlog from locking campaign_jobs.
	const limit = 2
	n, err := repo.PruneTerminalJobs(ctx, time.Hour, limit)
	if err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}
	if n > limit {
		t.Fatalf("one pass deleted %d rows with a limit of %d: an unbounded delete can lock "+
			"campaign_jobs and blow up the transaction on a large backlog", n, limit)
	}
	if got := surviving(); got < total-limit {
		t.Fatalf("one bounded pass left %d of this test's %d rows; it deleted more than the bound allows", got, total)
	}

	// The remainder must still be reachable — a backlog DRAINS across passes rather than being
	// abandoned. A bound that stopped making progress would let the table grow just as surely
	// as no bound at all.
	for range total {
		if surviving() == 0 {
			break
		}
		if _, err := repo.PruneTerminalJobs(ctx, time.Hour, limit); err != nil {
			t.Fatalf("PruneTerminalJobs (drain): %v", err)
		}
	}
	if got := surviving(); got != 0 {
		t.Errorf("%d of this test's rows survived repeated bounded passes; the backlog does not drain", got)
	}
}

// TestLivePruneTerminalJobsTreatsAZeroWindowAsTheDefault covers the argument the SWEEPER
// actually passes on an unconfigured deployment.
//
// StartJobRetentionSweeper calls PruneTerminalJobs with o.jobRetention, which is 0 whenever
// CAMPAIGN_JOB_RETENTION is unset, empty, or unparseable — the common case. Zero must select
// the long DefaultJobRetention. If it were instead handed to the interval arithmetic as-is,
// `updated_at < now() - '0s'` matches EVERY terminal row, and the first sweep on a default
// deployment would delete the entire spend history.
//
// The same applies to a zero limit, which the sweeper also passes: it must select the batch
// bound, not "unbounded".
func TestLivePruneTerminalJobsTreatsAZeroWindowAsTheDefault(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewJobRepo(&postgres.Pool{Pool: pool})

	briefID, _ := insertApprovedBrief(ctx, t, pool)

	// Terminal and old by any ordinary standard — but far younger than the 180-day default.
	id := insertJobAged(ctx, t, pool, briefID, model.JobSucceeded, 30*24*time.Hour)

	// Exactly the call the sweeper makes when nothing is configured.
	if _, err := repo.PruneTerminalJobs(ctx, 0, 0); err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}

	if !jobExists(ctx, t, pool, id) {
		t.Errorf("a 30-day-old terminal job was deleted by a zero-window prune: an unset "+
			"CAMPAIGN_JOB_RETENTION must select the %s default, not 'retain nothing' — this "+
			"would wipe the paid-campaign audit trail on every default deployment",
			postgres.DefaultJobRetention)
	}
}

// TestLivePruneTerminalJobsPrunesOnTerminalTimeNotCreationTime pins WHICH timestamp the window
// is measured against.
//
// A job created long ago but completed recently is RECENT history. Measuring the window on
// created_at would delete it while its terminal state is days old — the opposite of what
// retention means — and every other test here would stay green, because in them the two
// columns are set to the same instant.
func TestLivePruneTerminalJobsPrunesOnTerminalTimeNotCreationTime(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewJobRepo(&postgres.Pool{Pool: pool})

	briefID, _ := insertApprovedBrief(ctx, t, pool)

	// Created 30 days ago, but only reached 'succeeded' an hour ago.
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_jobs (brief_id, status, created_at, updated_at)
		VALUES ($1, 'succeeded', now() - interval '30 days', now() - interval '1 hour')
		RETURNING id::text`, briefID).Scan(&id)
	if err != nil {
		t.Fatalf("insert long-lived job: %v", err)
	}

	if _, err := repo.PruneTerminalJobs(ctx, 24*time.Hour, 0); err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}
	if !jobExists(ctx, t, pool, id) {
		t.Error("a job that reached its terminal state an hour ago was pruned on the strength of " +
			"its 30-day-old created_at; the window must be measured on updated_at")
	}
}

// TestLiveRetentionIndexServesThePrune pins the migration's index to the query it exists for.
//
// The prune runs on every replica forever, and the pre-existing idx_campaign_jobs_recovery is a
// partial index over the NON-terminal statuses — the exact complement of the rows this prune
// touches — so without a matching index the statement falls back to a full scan of the very
// history it is meant to bound. Asserted through EXPLAIN rather than by reading the migration
// text, because only the planner can say whether the index is actually usable for the
// predicate as written.
func TestLiveRetentionIndexServesThePrune(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)

	// EXPLAIN the prune's INNER SELECT — the statement the index exists for — rather than
	// asking pg_indexes whether an index by that name is present. An index can exist and
	// still not be usable for this predicate (wrong key column, a partial WHERE that does
	// not imply the query's status clause), and only the planner can say which.
	//
	// enable_seqscan=off is essential and is NOT a way of forcing a pass. On a test-sized
	// campaign_jobs a sequential scan is genuinely the cheapest plan, so an EXPLAIN of the
	// default plan would report Seq Scan with the index perfectly correct — the assertion
	// would fail on row count rather than on indexing. Disabling seqscan asks the question
	// that actually matters and holds at any table size: CAN the planner use this index for
	// this predicate? The negative control is real — with idx_campaign_jobs_retention
	// dropped, this same EXPLAIN still reports Seq Scan, because the only other candidate
	// (idx_campaign_jobs_recovery) is partial over the complementary statuses.
	//
	// A transaction so SET LOCAL is scoped to it and reverts on rollback, and so the SET and
	// the EXPLAIN are guaranteed to run on the SAME pooled connection.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	rows, err := tx.Query(ctx, `
		EXPLAIN (COSTS OFF, FORMAT TEXT)
		SELECT id FROM campaign_jobs
		WHERE status = ANY($1::text[]) AND updated_at < now() - $2::interval
		ORDER BY updated_at
		LIMIT 1000`,
		terminalStatusStrings(), postgres.DefaultJobRetention.String())
	if err != nil {
		t.Fatalf("EXPLAIN the prune's inner select: %v", err)
	}
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan EXPLAIN output: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read EXPLAIN output: %v", err)
	}
	plan := b.String()

	if !strings.Contains(plan, "idx_campaign_jobs_retention") {
		t.Fatalf("the planner will not use idx_campaign_jobs_retention for the prune's "+
			"predicate — the prune full-scans the very history it exists to bound, on every "+
			"replica, forever:\n%s", plan)
	}

	// The status clause must be absorbed by the index's own partial predicate, leaving no
	// residual Filter. A recheck here would mean the index covers more rows than the prune
	// deletes — it would still "be used", while scanning rows it cannot possibly return.
	if strings.Contains(plan, "Filter:") {
		t.Errorf("the index scan carries a residual Filter, so the partial predicate does not "+
			"imply the prune's status clause:\n%s", plan)
	}
}

// terminalStatusStrings is the prune's allow-list as the driver sends it, derived from the
// domain rather than restated — the same source TestTerminalJobStatusesMatchTheDomainVocabulary
// pins the repository's copy against.
func terminalStatusStrings() []string {
	var out []string
	for _, s := range model.AllJobStatuses {
		if s.Terminal() {
			out = append(out, string(s))
		}
	}
	return out
}
