// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeReconcileDB drives tryReleaseAudienceBuildLease one statement at a time.
//
// The live tests cover every outcome that can be staged against a real database. The two this
// file exists for cannot be: both need the row to become visible BETWEEN an attempt's UPDATE
// and its classifying SELECT, which is a window measured in microseconds and which nothing can
// schedule into. A fake is the only way to assert what the code does once it is inside that
// window — and the second-pass failure inside it is where the outcome was being reported wrong.
//
// It models the real contract rather than a convenient one: Exec returns a CommandTag whose
// RowsAffected the code reads, and QueryRow returns a pgx.Row whose Scan can report
// pgx.ErrNoRows, so the classification switch is exercised as written.
type fakeReconcileDB struct {
	execs    []fakeReleaseExec
	rows     []fakeStatusRead
	execCall int
	rowCall  int
	t        *testing.T
}

type fakeReleaseExec struct {
	affected int64
	err      error
}

type fakeStatusRead struct {
	status string
	err    error
}

func (f *fakeReconcileDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	if f.execCall >= len(f.execs) {
		f.t.Fatalf("Exec called %d times, more than the %d outcomes the test staged", f.execCall+1, len(f.execs))
	}
	e := f.execs[f.execCall]
	f.execCall++
	if e.err != nil {
		return pgconn.CommandTag{}, e.err
	}
	// pgconn builds RowsAffected by parsing the command tag text, so the tag has to look like
	// one. A zero-value CommandTag would report 0 for every case and make a passing test vacuous.
	return pgconn.NewCommandTag("UPDATE " + strconv.FormatInt(e.affected, 10)), nil
}

func (f *fakeReconcileDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if f.rowCall >= len(f.rows) {
		f.t.Fatalf("QueryRow called %d times, more than the %d outcomes the test staged", f.rowCall+1, len(f.rows))
	}
	r := f.rows[f.rowCall]
	f.rowCall++
	return &fakeStatusRow{row: r}
}

type fakeStatusRow struct{ row fakeStatusRead }

func (r *fakeStatusRow) Scan(dest ...any) error {
	if r.row.err != nil {
		return r.row.err
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("fake row: the reconcile scanned something other than a *string status")
	}
	*p = r.row.status
	return nil
}

func reconcileProbeRow() *model.CampaignAudience {
	return &model.CampaignAudience{
		ID:        "aud-1",
		BriefID:   "brief-1",
		ProjectID: "cncf",
		Platform:  model.ProviderHubSpot,
	}
}

// TestTryReleaseAudienceBuildLease_KeepsHeldAfterASecondPassFailure pins the Copilot finding on
// PR #106.
//
// Once the first pass has read the row as 'building', this process KNOWS the (brief_id,
// platform) lease is held. A query that then FAILS on the second pass does not unlearn that —
// but both error returns handed back audienceReconcileUnseen, the zero value, so the retry
// loop's confirmedHeld stayed false and reportUnreconciledAudience logged a confirmed stranded
// lease at warn with the hedged "if the commit did land" wording. That is precisely the
// downgrade the tri-state outcome was introduced to prevent, reintroduced through the error
// path.
//
// Both second-pass statements get a case, because the fix has to hold for either: the release
// Exec and the classifying SELECT fail for the same reason (an unreliable connection) and are
// equally uninformative about whether the lease is held.
func TestTryReleaseAudienceBuildLease_KeepsHeldAfterASecondPassFailure(t *testing.T) {
	boom := errors.New("connection reset by peer")

	tests := map[string]struct {
		db *fakeReconcileDB
	}{
		"the second-pass release fails": {
			db: &fakeReconcileDB{
				// Pass 0: UPDATE matches nothing, SELECT says the row is there and 'building'.
				// Pass 1: the UPDATE itself errors.
				execs: []fakeReleaseExec{{affected: 0}, {err: boom}},
				rows:  []fakeStatusRead{{status: "building"}},
			},
		},
		"the second-pass status read fails": {
			db: &fakeReconcileDB{
				execs: []fakeReleaseExec{{affected: 0}, {affected: 0}},
				rows:  []fakeStatusRead{{status: "building"}, {err: boom}},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.db.t = t
			outcome, err := tryReleaseAudienceBuildLease(context.Background(), tc.db, reconcileProbeRow())
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the staged failure wrapped — the caller logs it", err)
			}
			if outcome != audienceReconcileHeld {
				t.Errorf("outcome = %d, want audienceReconcileHeld (%d). The first pass already read "+
					"the row as 'building', so the lease is confirmed held; reporting Unseen (%d) here "+
					"leaves confirmedHeld false and downgrades a known stranded lease to the ordinary "+
					"probable-rollback warn", outcome, audienceReconcileHeld, audienceReconcileUnseen)
			}
		})
	}
}

// TestTryReleaseAudienceBuildLease_ReportsUnseenWhenNothingWasObserved is the other half of the
// same invariant, and the reason the fix promotes rather than hard-codes. A FIRST-pass failure
// has observed nothing at all, so it must still classify as Unseen — the overwhelmingly likely
// reading is the ordinary rolled-back commit, and reporting every one of those as a confirmed
// stranded lease at error is how the error stops being read.
func TestTryReleaseAudienceBuildLease_ReportsUnseenWhenNothingWasObserved(t *testing.T) {
	boom := errors.New("connection reset by peer")

	tests := map[string]struct {
		db *fakeReconcileDB
	}{
		"the first-pass release fails": {
			db: &fakeReconcileDB{execs: []fakeReleaseExec{{err: boom}}},
		},
		"the first-pass status read fails": {
			db: &fakeReconcileDB{
				execs: []fakeReleaseExec{{affected: 0}},
				rows:  []fakeStatusRead{{err: boom}},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.db.t = t
			outcome, err := tryReleaseAudienceBuildLease(context.Background(), tc.db, reconcileProbeRow())
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the staged failure wrapped", err)
			}
			if outcome != audienceReconcileUnseen {
				t.Errorf("outcome = %d, want audienceReconcileUnseen (%d) — nothing was observed, so "+
					"there is no held lease to report", outcome, audienceReconcileUnseen)
			}
		})
	}
}

// TestTryReleaseAudienceBuildLease_HeldAfterBothPassesSeeBuilding covers the outcome the live
// tests document as unschedulable: two full passes, the row visible and 'building' throughout,
// no error anywhere. The second pass exists because a row that appears between an attempt's two
// statements is released by an immediate retry; when even that does not match, Held is the
// honest answer.
func TestTryReleaseAudienceBuildLease_HeldAfterBothPassesSeeBuilding(t *testing.T) {
	db := &fakeReconcileDB{
		t:     t,
		execs: []fakeReleaseExec{{affected: 0}, {affected: 0}},
		rows:  []fakeStatusRead{{status: "building"}, {status: "building"}},
	}
	outcome, err := tryReleaseAudienceBuildLease(context.Background(), db, reconcileProbeRow())
	if err != nil {
		t.Fatalf("no statement failed, so there is nothing to report: %v", err)
	}
	if outcome != audienceReconcileHeld {
		t.Errorf("outcome = %d, want audienceReconcileHeld (%d)", outcome, audienceReconcileHeld)
	}
	if db.execCall != 2 || db.rowCall != 2 {
		t.Errorf("execs=%d reads=%d, want 2 and 2 — the second pass is what closes the "+
			"became-visible-between-statements window", db.execCall, db.rowCall)
	}
}

// TestTryReleaseAudienceBuildLease_SettlesOnTheSecondPass pins the second pass's whole purpose:
// a row that becomes visible between the first pass's two statements is released immediately
// rather than left to a next attempt that, on the last one, does not exist.
func TestTryReleaseAudienceBuildLease_SettlesOnTheSecondPass(t *testing.T) {
	db := &fakeReconcileDB{
		t:     t,
		execs: []fakeReleaseExec{{affected: 0}, {affected: 1}},
		rows:  []fakeStatusRead{{status: "building"}},
	}
	outcome, err := tryReleaseAudienceBuildLease(context.Background(), db, reconcileProbeRow())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != audienceReconcileSettled {
		t.Errorf("outcome = %d, want audienceReconcileSettled (%d) — the second UPDATE matched",
			outcome, audienceReconcileSettled)
	}
}

// TestAmbiguousCommitReconcileScheduleSpans computes the schedule from the three constants that
// define it, so the comment above them cannot drift from the code again.
//
// It did drift: the comment claimed "~1.2s" while 6 attempts sleep 5 times for
// 25+50+100+200+400 = 775ms, and never reach the documented 500ms cap at all. Copilot found it
// on PR #106. Asserting the sum against a hand-written comment would have caught nothing, so
// the test derives it the way the loop does — N attempts sleep N-1 times, doubling from the
// minimum, clamped at the maximum.
func TestAmbiguousCommitReconcileScheduleSpans(t *testing.T) {
	var total time.Duration
	delay := ambiguousCommitReconcileMinDelay
	for attempt := 1; attempt < ambiguousCommitReconcileAttempts; attempt++ {
		total += delay
		if delay *= 2; delay > ambiguousCommitReconcileMaxDelay {
			delay = ambiguousCommitReconcileMaxDelay
		}
	}

	const want = 1275 * time.Millisecond
	if total != want {
		t.Errorf("the retry schedule spans %v, want %v. Whichever of the three constants changed, "+
			"the paragraph above them describes the old schedule; update both", total, want)
	}
	if total >= ambiguousCommitReconcileTimeout {
		t.Errorf("the retry schedule spans %v, at or past the %v reconcile timeout — the attempt "+
			"cap is supposed to be what normally ends the loop, so that the timeout stays a ceiling "+
			"for slow queries rather than the ordinary exit", total, ambiguousCommitReconcileTimeout)
	}
	// The cap has to be reachable or it is not the schedule the comment describes.
	if ambiguousCommitReconcileAttempts < 2 {
		t.Fatal("fewer than two attempts never sleeps at all")
	}
	reached := ambiguousCommitReconcileMinDelay
	for attempt := 1; attempt < ambiguousCommitReconcileAttempts-1; attempt++ {
		reached *= 2
	}
	if reached < ambiguousCommitReconcileMaxDelay {
		t.Errorf("the last delay is %v and never reaches the %v cap, so the schedule stops short of "+
			"the one it documents", reached, ambiguousCommitReconcileMaxDelay)
	}
}
