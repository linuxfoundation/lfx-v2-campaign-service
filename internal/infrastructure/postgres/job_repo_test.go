// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestTerminalJobStatusesMatchTheDomainVocabulary pins the prune's allow-list against
// model.JobStatus.Terminal() over the WHOLE status vocabulary, in both directions.
//
// The allow-list is a hand-written copy of a decision that lives in the domain model, and the
// two can drift silently: nothing else compares them, and every other test here would stay
// green if they did. The consequence of drift is asymmetric and severe in one direction — a
// status wrongly listed as terminal starts DELETING rows that record real ad spend.
//
// Iterating the full vocabulary (rather than asserting the three names) is what makes this
// catch a status ADDED later: a new 'cancelled' that the model calls terminal and the prune
// does not, or vice versa, fails here instead of being discovered in production.
func TestTerminalJobStatusesMatchTheDomainVocabulary(t *testing.T) {
	all := []model.JobStatus{
		model.JobQueued, model.JobRunning,
		model.JobSucceeded, model.JobPartial, model.JobFailed,
	}

	listed := make(map[string]bool, len(terminalJobStatuses))
	for _, s := range terminalJobStatuses {
		listed[s] = true
	}

	for _, s := range all {
		inList := listed[string(s)]
		switch {
		case s.Terminal() && !inList:
			t.Errorf("%q is terminal in the domain model but missing from the prune allow-list: "+
				"its rows would be retained forever", s)
		case !s.Terminal() && inList:
			t.Errorf("%q is NOT terminal in the domain model but appears in the prune allow-list: "+
				"the prune would delete the audit trail of a job that never finished", s)
		}
	}

	// Guard the copy itself: a duplicate or an unknown value here means the list no longer
	// describes the vocabulary it claims to.
	if len(listed) != len(terminalJobStatuses) {
		t.Errorf("terminalJobStatuses contains duplicates: %v", terminalJobStatuses)
	}
	known := make(map[string]bool, len(all))
	for _, s := range all {
		known[string(s)] = true
	}
	for _, s := range terminalJobStatuses {
		if !known[s] {
			t.Errorf("terminalJobStatuses contains %q, which is not a known JobStatus", s)
		}
	}
}

// TestPruneTerminalJobsQueryUsesAnAllowList pins the SHAPE of the predicate, not just its
// result.
//
// A negative predicate (status != 'running', status NOT IN ('queued','running')) would pass
// every behavioural test written against today's five statuses, then silently start deleting a
// status added later. The distinction only exists in the SQL text, so that is where it is
// asserted.
func TestPruneTerminalJobsQueryUsesAnAllowList(t *testing.T) {
	q := pruneTerminalJobsQuery

	if strings.Contains(q, "!=") || strings.Contains(q, "<>") || strings.Contains(q, "NOT IN") {
		t.Errorf("the prune predicate is negative; a status added later would be swept in "+
			"silently, deleting records of real ad spend:\n%s", q)
	}
	if !strings.Contains(q, "status = ANY(") {
		t.Errorf("the prune must select statuses from an explicit allow-list parameter:\n%s", q)
	}
	// The bound and its ordering are what keep one pass short.
	if !strings.Contains(q, "LIMIT") {
		t.Errorf("the prune must bound each batch:\n%s", q)
	}
	if !strings.Contains(q, "ORDER BY updated_at") {
		t.Errorf("the prune must take the OLDEST rows first, ordered by the terminal-time column:\n%s", q)
	}
	// Age must be measured on the terminal transition, never on creation.
	if strings.Contains(q, "created_at") {
		t.Errorf("the prune must measure age on updated_at (when the job reached its terminal "+
			"state), not created_at:\n%s", q)
	}
	// Hand-written SQL with $N placeholders — no interpolation of the window or the bound.
	for _, ph := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(q, ph) {
			t.Errorf("expected placeholder %s in the prune query:\n%s", ph, q)
		}
	}
}

// TestDefaultJobRetentionIsSafe pins the DIRECTION of the default.
//
// These rows are the audit trail of real money being spent. A default that drifted short would
// quietly start deleting spend history on every deployment that never sets the variable, which
// is the common case. The exact figure is a judgement call; that it is measured in months is
// not.
func TestDefaultJobRetentionIsSafe(t *testing.T) {
	if DefaultJobRetention < 90*24*time.Hour {
		t.Errorf("DefaultJobRetention is %s: the default retention for paid-campaign audit "+
			"records must be long, since a deployment that sets nothing gets it", DefaultJobRetention)
	}
}

// TestRetentionMigrationIndexesOnlyTerminalRows reads migration 000026 and pins the partial
// index to the prune's predicate.
//
// The pre-existing idx_campaign_jobs_recovery (000004) is partial over queued/running — the
// exact COMPLEMENT of these rows — so it cannot serve this statement. Without a matching index
// the prune full-scans the unbounded history it exists to bound, on every replica, forever.
func TestRetentionMigrationIndexesOnlyTerminalRows(t *testing.T) {
	sql, err := os.ReadFile(filepath.Join("migrations", "000026_campaign_jobs_retention_index.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	// Assert over the STATEMENT, not the file. The migration's comment necessarily discusses
	// the non-terminal statuses (it explains why 000004's index cannot serve this query), and
	// a whole-file scan would read that prose as part of the predicate.
	stmt := statementsOf(string(sql))

	if !strings.Contains(stmt, "idx_campaign_jobs_retention") {
		t.Fatalf("migration 000026 does not create idx_campaign_jobs_retention:\n%s", stmt)
	}
	if !strings.Contains(stmt, "(updated_at)") {
		t.Errorf("the retention index must be keyed on updated_at, the column the prune orders and filters on:\n%s", stmt)
	}
	for _, s := range []model.JobStatus{model.JobSucceeded, model.JobPartial, model.JobFailed} {
		if !strings.Contains(stmt, "'"+string(s)+"'") {
			t.Errorf("the retention index predicate omits terminal status %q:\n%s", s, stmt)
		}
	}
	for _, s := range []model.JobStatus{model.JobQueued, model.JobRunning} {
		if strings.Contains(stmt, "'"+string(s)+"'") {
			t.Errorf("the retention index predicate must not cover non-terminal status %q:\n%s", s, stmt)
		}
	}
}

// statementsOf strips whole-line SQL comments, leaving the executable statements. The migration
// files carry long rationale comments that mention identifiers the statements deliberately do
// not use.
func statementsOf(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
