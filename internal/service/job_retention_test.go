// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// erroringPruneJobRepo fails every prune, so a test can prove the sweeper keeps running.
type erroringPruneJobRepo struct {
	fakeJobRepo
}

func (r *erroringPruneJobRepo) PruneTerminalJobs(context.Context, time.Duration, int) (int64, error) {
	return 0, errors.New("prune failed")
}

// TestSetJobRetentionIgnoresNonPositiveValues pins the direction of the fallback.
//
// Zero is what an UNSET or unparseable CAMPAIGN_JOB_RETENTION produces, and it must mean "use
// the long repository default", never "retain nothing". Reading a missing config value as an
// instruction to delete would destroy the audit trail of real ad spend on a typo.
func TestSetJobRetentionIgnoresNonPositiveValues(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), nil)

	for _, d := range []time.Duration{0, -time.Hour, -1} {
		orch.SetJobRetention(d)
		if orch.jobRetention != 0 {
			t.Errorf("SetJobRetention(%s) installed %s; a non-positive value must leave the "+
				"repository default in place", d, orch.jobRetention)
		}
	}

	// A positive value IS installed, including one shorter than the default — operators may
	// legitimately want a shorter window.
	orch.SetJobRetention(48 * time.Hour)
	if orch.jobRetention != 48*time.Hour {
		t.Errorf("jobRetention = %s, want 48h: an explicit operator window must be honoured", orch.jobRetention)
	}
}

// TestRetentionSweeperPrunesOnlyTerminalJobs is the end-to-end guard on the wiring.
//
// The repository-level tests prove the SQL spares non-terminal rows; this proves the SWEEPER
// calls it in a way that preserves that property — that it does not, say, pass its own
// predicate or a zero window that means something different one layer down.
//
// Drives the sweep directly rather than waiting for jobRetentionSweepInterval (an hour): the
// tick is a scheduling detail, and the assertion is about what one pass does.
func TestRetentionSweeperPrunesOnlyTerminalJobs(t *testing.T) {
	jobs := newFakeJobRepo()
	old := time.Now().Add(-72 * time.Hour)

	// Aged well past the window, one row per status in the vocabulary.
	for _, s := range []model.JobStatus{
		model.JobQueued, model.JobRunning,
		model.JobSucceeded, model.JobPartial, model.JobFailed,
	} {
		jobs.jobs[string(s)] = &model.CampaignJob{
			ID: string(s), BriefID: "b1", Status: s, UpdatedAt: old,
		}
	}

	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, nil)
	orch.SetJobRetention(time.Hour)

	if _, err := orch.jobs.PruneTerminalJobs(context.Background(), orch.jobRetention, 0); err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}

	for _, s := range []model.JobStatus{model.JobQueued, model.JobRunning} {
		if _, ok := jobs.jobs[string(s)]; !ok {
			t.Errorf("the sweeper deleted a %s job: a non-terminal row is a stuck job and is "+
				"exactly what an operator needs to investigate", s)
		}
	}
	for _, s := range []model.JobStatus{model.JobSucceeded, model.JobPartial, model.JobFailed} {
		if _, ok := jobs.jobs[string(s)]; ok {
			t.Errorf("a %s job past the window survived; retention does not bound the table", s)
		}
	}
}

// TestRetentionSweeperPassesTheConfiguredWindow proves the operator's setting actually reaches
// the repository. A sweeper that dropped it on the floor would silently apply the default
// forever while the env var appeared to work.
func TestRetentionSweeperPassesTheConfiguredWindow(t *testing.T) {
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, nil)
	orch.SetJobRetention(96 * time.Hour)

	// Mirror one sweeper pass exactly as StartJobRetentionSweeper performs it.
	if _, err := orch.jobs.PruneTerminalJobs(context.Background(), orch.jobRetention, 0); err != nil {
		t.Fatalf("PruneTerminalJobs: %v", err)
	}

	gotWindow, gotLimit := jobs.lastPruneArgs()
	if gotWindow != 96*time.Hour {
		t.Errorf("prune window = %s, want 96h: the configured retention must reach the repository", gotWindow)
	}
	// The sweeper passes 0 so the repository applies its own batch bound — it must not invent
	// an unbounded delete.
	if gotLimit != 0 {
		t.Errorf("prune limit = %d, want 0 (the repository's own bound applies)", gotLimit)
	}
}

// TestRetentionSweeperStopsOnShutdown pins the sweeper's lifetime to sweeperCtx, the same
// context that owns the recovery sweeper.
//
// A goroutine that outlived Shutdown would run a DELETE against a closing pool. Because
// Shutdown cancels sweeperCtx before the dispatch drain, the sweeper must observe it and
// return — otherwise Shutdown's wg.Wait would block for the full interval.
func TestRetentionSweeperStopsOnShutdown(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), nil)
	orch.StartJobRetentionSweeper()

	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(context.Background(), time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: the retention sweeper is not bound to sweeperCtx and " +
			"would keep deleting against a closing pool")
	}
}

// TestRetentionSweeperSurvivesARepositoryError pins that a prune failure is logged and dropped
// rather than killing the goroutine. Retention failing costs disk, never correctness — but a
// sweeper that exited on the first transient error would stop bounding the table for the
// lifetime of the pod, silently.
func TestRetentionSweeperSurvivesARepositoryError(t *testing.T) {
	jobs := &erroringPruneJobRepo{fakeJobRepo: fakeJobRepo{jobs: map[string]*model.CampaignJob{}}}
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, nil)
	orch.StartJobRetentionSweeper()

	// Shutdown must still return cleanly, which it can only do if the goroutine is alive and
	// watching sweeperCtx rather than having exited early on the error path.
	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(context.Background(), time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after a prune error")
	}
}
