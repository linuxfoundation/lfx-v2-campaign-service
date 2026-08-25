// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// PostgreSQL SQLSTATE codes asserted below. Named rather than inlined so a reader does not
// have to decode a five-character literal, and so the two negative tests cannot drift onto
// the same code by a copy-paste.
const (
	pgerrcodeForeignKeyViolation = "23503"
	pgerrcodeCheckViolation      = "23514"
)

// The job repository's status transitions have been asserted only over SQL source text.
// job_retention_live_test.go does drive PruneTerminalJobs live, but the transitions that
// PUT a job into a terminal state — CreateJob, UpdateJobStatus, FailStuckJobs — and the
// tenant-scoped GetJob read are never invoked against a live database.
//
// Two things here are only observable live. First, campaign_jobs.status carries a CHECK
// constraint listing the five legal values; a status the Go vocabulary allows but the
// constraint does not would be rejected at runtime by PostgreSQL and by nothing else.
// Second, GetJob's tenant scoping is a JOIN through campaign_briefs, so a test over the
// statement text cannot show whether the join actually excludes another project's job.

// newJobRepo builds a JobRepo over the live pool.
func newJobRepo(pool *pgxpool.Pool) *postgres.JobRepo {
	return postgres.NewJobRepo(&postgres.Pool{Pool: pool})
}

// TestLiveCreateAndGetJobRoundTrip covers create/get for jobs. A new job must land in
// 'queued' — that is a column DEFAULT, not a value the insert supplies, so this is the
// only place it is checked against the real table.
func TestLiveCreateAndGetJobRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newJobRepo(pool)

	briefID, project := insertApprovedBrief(ctx, t, pool)

	job, err := repo.CreateJob(ctx, briefID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.ID == "" {
		t.Error("CreateJob returned an empty id; the RETURNING clause is not yielding the generated key")
	}
	if job.Status != model.JobQueued {
		t.Errorf("new job status = %q, want %q", job.Status, model.JobQueued)
	}
	if job.BriefID != briefID {
		t.Errorf("job brief_id = %q, want %q", job.BriefID, briefID)
	}
	// A fresh job has neither a result nor an error. nullBytes/nullStr map the empty
	// values to SQL NULL, and scanJob must bring them back as zero values rather than
	// failing to scan a NULL into a non-pointer destination.
	if len(job.Result) != 0 {
		t.Errorf("new job result = %s, want empty", job.Result)
	}
	if job.Error != "" {
		t.Errorf("new job error = %q, want empty", job.Error)
	}

	got, err := repo.GetJob(ctx, project, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ID != job.ID || got.BriefID != briefID || got.Status != model.JobQueued {
		t.Errorf("GetJob = %+v, want the job just created (%s)", got, job.ID)
	}
}

// TestLiveGetJobIsScopedToTheOwningProject pins the JOIN in GetJob.
//
// The route is /projects/{project_id}/jobs/{job_id}, so a job UUID must not be a bearer
// capability across tenants. campaign_jobs has NO project_id column of its own — the scope
// comes entirely from joining to the owning brief — which is exactly why dropping the join
// predicate is an easy edit that no source-text test would catch.
func TestLiveGetJobIsScopedToTheOwningProject(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newJobRepo(pool)

	briefID, project := insertApprovedBrief(ctx, t, pool)
	// A second, genuinely separate tenant with its own brief, so the "foreign project"
	// in the assertion below is a project that really exists rather than a random string.
	_, otherProject := insertApprovedBrief(ctx, t, pool)

	job, err := repo.CreateJob(ctx, briefID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if _, err := repo.GetJob(ctx, otherProject, job.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetJob for a job owned by another project = %v, want domain.ErrNotFound", err)
	}
	// The owner can still read it, so the assertion above is about tenancy rather than
	// about the job having failed to insert.
	if _, err := repo.GetJob(ctx, project, job.ID); err != nil {
		t.Fatalf("GetJob by the owning project: %v", err)
	}
	// An absent job id is ErrNotFound rather than a scan error.
	if _, err := repo.GetJob(ctx, project, "00000000-0000-4000-8000-00000000f00d"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetJob on an absent id = %v, want domain.ErrNotFound", err)
	}
}

// TestLiveUpdateJobStatusWalksEveryLegalTransition drives a job through the real lifecycle
// and asserts each status PERSISTS.
//
// Every value in model.AllJobStatuses is exercised against the live column because
// campaign_jobs.status carries a CHECK constraint enumerating the five legal values. A
// status added to the Go vocabulary but not to the constraint fails at runtime with SQLSTATE
// 23514 and is invisible to every in-memory test — this walk is what surfaces that drift.
func TestLiveUpdateJobStatusWalksEveryLegalTransition(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newJobRepo(pool)

	briefID, project := insertApprovedBrief(ctx, t, pool)
	job, err := repo.CreateJob(ctx, briefID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// queued -> running. No result or error yet: the dispatch is in flight.
	if err := repo.UpdateJobStatus(ctx, job.ID, model.JobRunning, nil, ""); err != nil {
		t.Fatalf("UpdateJobStatus to running: %v", err)
	}
	running, err := repo.GetJob(ctx, project, job.ID)
	if err != nil {
		t.Fatalf("GetJob after running: %v", err)
	}
	if running.Status != model.JobRunning {
		t.Errorf("status after update = %q, want %q: the transition did not persist", running.Status, model.JobRunning)
	}

	// running -> succeeded, carrying a result document. The result must survive the jsonb
	// column: a dispatch's per-platform outcome is the only in-service record of what was
	// created upstream.
	result := json.RawMessage(`{"google_ads":{"campaign_id":"777"}}`)
	if err := repo.UpdateJobStatus(ctx, job.ID, model.JobSucceeded, result, ""); err != nil {
		t.Fatalf("UpdateJobStatus to succeeded: %v", err)
	}
	succeeded, err := repo.GetJob(ctx, project, job.ID)
	if err != nil {
		t.Fatalf("GetJob after succeeded: %v", err)
	}
	if succeeded.Status != model.JobSucceeded {
		t.Errorf("status = %q, want %q", succeeded.Status, model.JobSucceeded)
	}
	assertJSONEqual(t, "result", succeeded.Result, `{"google_ads":{"campaign_id":"777"}}`)

	// Every status in the vocabulary must be accepted by the CHECK constraint. Driving each
	// one through the real UPDATE is the only way to learn that the Go vocabulary and the
	// constraint still agree.
	//
	// Derived from model.AllJobStatuses rather than hand-copied, which is what that variable
	// exists for: a test carrying its own list keeps agreeing with its own copy while a
	// status added later goes unexercised — and an unexercised status is exactly one that
	// might be missing from the CHECK constraint.
	for _, status := range model.AllJobStatuses {
		if err := repo.UpdateJobStatus(ctx, job.ID, status, nil, "boom"); err != nil {
			t.Fatalf("UpdateJobStatus to %q rejected by the live column: %v", status, err)
		}
		got, gerr := repo.GetJob(ctx, project, job.ID)
		if gerr != nil {
			t.Fatalf("GetJob after %q: %v", status, gerr)
		}
		if got.Status != status {
			t.Errorf("status = %q, want %q", got.Status, status)
		}
		// The error text is written in the SAME statement as the status, so a terminal
		// failure can never be committed without its explanation.
		if got.Error != "boom" {
			t.Errorf("error after transition to %q = %q, want %q", status, got.Error, "boom")
		}
		// result is written UNCONDITIONALLY (`result=$2`), and nullBytes maps an empty
		// slice to SQL NULL — so passing nil here CLEARS the document stored above rather
		// than leaving it in place. That is real behaviour a caller depends on (a retry
		// must not inherit the previous attempt's per-platform outcome), and it is not
		// visible anywhere else in this walk.
		if len(got.Result) != 0 {
			t.Errorf("result after transition to %q = %s, want it cleared: a nil result must "+
				"not leave the previous attempt's document in place", status, got.Result)
		}
	}

	// An update against an absent job must report ErrNotFound rather than succeeding
	// silently: UpdateJobStatus has no RETURNING clause, so RowsAffected()==0 is the only
	// signal that the write hit nothing.
	if err := repo.UpdateJobStatus(ctx, "00000000-0000-4000-8000-00000000abcd", model.JobFailed, nil, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateJobStatus on an absent id = %v, want domain.ErrNotFound", err)
	}
}

// TestLiveUpdateJobStatusRejectsAStatusOutsideTheCheckConstraint proves the CHECK
// constraint is REALLY there and really enforced.
//
// Without this, the walk above only shows that five known-good values are accepted — which
// a column with no constraint at all would also satisfy. The negative case is what
// distinguishes "the constraint holds" from "there is no constraint".
func TestLiveUpdateJobStatusRejectsAStatusOutsideTheCheckConstraint(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newJobRepo(pool)

	briefID, project := insertApprovedBrief(ctx, t, pool)
	job, err := repo.CreateJob(ctx, briefID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// 'cancelled' is a plausible future status that is NOT in the constraint today.
	//
	// Assert the SQLSTATE, not merely that some error came back: a connection failure, a
	// malformed id or a scan error would all satisfy a bare `err != nil` and read as
	// "the constraint is enforced" while proving nothing about the constraint.
	err = repo.UpdateJobStatus(ctx, job.ID, model.JobStatus("cancelled"), nil, "")
	if err == nil {
		t.Fatal("UpdateJobStatus accepted a status outside the CHECK constraint; campaign_jobs.status is not constrained")
	}
	var checkErr *pgconn.PgError
	if !errors.As(err, &checkErr) || checkErr.Code != pgerrcodeCheckViolation {
		t.Fatalf("UpdateJobStatus with an illegal status = %v, want SQLSTATE %s (check_violation)", err, pgerrcodeCheckViolation)
	}

	// The rejected write must not have changed the row.
	got, err := repo.GetJob(ctx, project, job.ID)
	if err != nil {
		t.Fatalf("GetJob after the rejected update: %v", err)
	}
	if got.Status != model.JobQueued {
		t.Errorf("status after a rejected update = %q, want %q: the failed write was partially applied", got.Status, model.JobQueued)
	}
}

// TestLiveFailStuckJobsOnlySweepsIdleNonTerminalJobs covers the recovery sweep.
//
// Three properties matter and all three need a live clock and a live table: it must fail
// jobs that have been idle past staleJobCutoff, it must SPARE a job that is merely recent
// (a job still being worked by another replica during a rolling deploy), and it must never
// touch a row that is already terminal — rewriting a succeeded job to 'failed' would
// destroy the record of a dispatch that actually worked.
//
// # This test must leave no aged non-terminal rows behind
//
// FailStuckJobs is TABLE-WIDE: it carries no tenant or brief predicate, so it rewrites every
// aged queued/running row in the shared schema, including rows another test owns. The hazard
// runs in both directions and is worth stating, because neither end is obvious from the other
// file:
//
//   - Outbound: TestLivePruneTerminalJobsSparesEveryNonTerminalRow seeds queued and running
//     jobs aged 72 hours and asserts they survive its prune. This sweep would flip them to
//     'failed' — which is TERMINAL — and a terminal aged row is precisely what that test's
//     prune then deletes.
//   - Inbound: any aged non-terminal row another test leaves behind is swept by THIS call and
//     inflates the returned count.
//
// The two do not collide today because each test seeds its rows and acts on them within its
// own body, and the package runs sequentially. That is a property of the current schedule, not
// a guarantee — this package already calls t.Parallel elsewhere (migrate_down_live_test.go).
//
// So this test cleans up after itself: every row it creates is deleted on exit, and the count
// assertion is expressed as a delta over rows this test owns rather than as a table-wide
// total. Both halves matter — the cleanup keeps this test from breaking others, and the delta
// keeps others from breaking this one.
func TestLiveFailStuckJobsOnlySweepsIdleNonTerminalJobs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newJobRepo(pool)

	briefID, project := insertApprovedBrief(ctx, t, pool)

	// Delete this test's jobs on the way out so the rows it deliberately ages can never be
	// swept into another test's fixture.
	//
	// Registered BEFORE the first insert, not after the last. insertJobAged fails the test
	// with t.Fatalf, so a failure on the second, third or fourth insert would otherwise
	// unwind past a cleanup that had not been registered yet and strand the aged rows
	// already committed — which is precisely the leak this cleanup exists to prevent, and
	// it would appear only on a day something else was already broken. Registering against
	// briefID alone is what makes this possible: the cleanup names the parent, so it does
	// not need any of the job ids to exist.
	t.Cleanup(func() {
		cctx, cancel := dbtest.CleanupContext()
		defer cancel()
		if _, err := pool.Exec(cctx, `DELETE FROM campaign_jobs WHERE brief_id = $1`, briefID); err != nil {
			t.Errorf("cleanup this test's jobs: %v", err)
		}
	})

	// Aged well beyond the 15-minute cutoff. insertJobAged backdates updated_at, which is
	// the column the sweep measures.
	stuckQueued := insertJobAged(ctx, t, pool, briefID, model.JobQueued, 2*time.Hour)
	stuckRunning := insertJobAged(ctx, t, pool, briefID, model.JobRunning, 2*time.Hour)
	// Recent: inside the cutoff, so still presumed live.
	freshRunning := insertJobAged(ctx, t, pool, briefID, model.JobRunning, time.Minute)
	// Already terminal AND old: the age predicate alone would match it, so this row is
	// what proves the status predicate is doing work.
	oldSucceeded := insertJobAged(ctx, t, pool, briefID, model.JobSucceeded, 2*time.Hour)

	const sweepErr = "recovered by startup sweep"
	n, err := repo.FailStuckJobs(ctx, sweepErr)
	if err != nil {
		t.Fatalf("FailStuckJobs: %v", err)
	}
	// The sweep is table-wide, so the raw count includes any aged non-terminal row another
	// test left behind. Assert a floor — this test's own two stuck jobs must be in it. The
	// binding evidence is the per-row assertions below, which name exactly which rows moved
	// and which did not; the count alone could not distinguish them.
	if n < 2 {
		t.Errorf("FailStuckJobs swept %d rows, want at least the 2 stuck jobs this test created", n)
	}

	statusOf := func(id string) *model.CampaignJob {
		t.Helper()
		j, gerr := repo.GetJob(ctx, project, id)
		if gerr != nil {
			t.Fatalf("GetJob(%s): %v", id, gerr)
		}
		return j
	}

	for _, id := range []string{stuckQueued, stuckRunning} {
		j := statusOf(id)
		if j.Status != model.JobFailed {
			t.Errorf("stuck job %s status = %q, want %q: an orphaned job stays non-terminal forever", id, j.Status, model.JobFailed)
		}
		// The sweep's explanation is written in the same statement, so an operator can
		// tell a swept job apart from one that failed on its own.
		if j.Error != sweepErr {
			t.Errorf("stuck job %s error = %q, want %q", id, j.Error, sweepErr)
		}
	}

	if j := statusOf(freshRunning); j.Status != model.JobRunning {
		t.Errorf("recent running job status = %q, want %q: the sweep failed a job that is still being worked", j.Status, model.JobRunning)
	}
	if j := statusOf(oldSucceeded); j.Status != model.JobSucceeded {
		t.Errorf("old succeeded job status = %q, want %q: the sweep overwrote a terminal job and destroyed the record of a real dispatch",
			j.Status, model.JobSucceeded)
	}
}

// TestLiveCreateJobRequiresARealBrief pins the foreign key. campaign_jobs.brief_id
// REFERENCES campaign_briefs(id), so a job can never be orphaned from the brief that
// explains what it was dispatching. Only the live table enforces this.
func TestLiveCreateJobRequiresARealBrief(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newJobRepo(pool)

	_, err := repo.CreateJob(ctx, "00000000-0000-4000-8000-0000000000fe")
	if err == nil {
		t.Fatal("CreateJob accepted a brief_id with no such brief; the foreign key is not enforced")
	}
	// The SQLSTATE, not just "an error": CreateJob wraps whatever it gets with %w and does
	// no FK-to-sentinel mapping, so a connection error would otherwise pass this test while
	// telling us nothing about the foreign key.
	var fkErr *pgconn.PgError
	if !errors.As(err, &fkErr) || fkErr.Code != pgerrcodeForeignKeyViolation {
		t.Fatalf("CreateJob against an absent brief = %v, want SQLSTATE %s (foreign_key_violation)", err, pgerrcodeForeignKeyViolation)
	}
}
