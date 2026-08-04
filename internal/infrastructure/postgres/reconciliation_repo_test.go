// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestNonSettledCampaignStatusesExcludesSettled pins which statuses the inventory
// treats as needing attention.
//
// The dangerous direction is INCLUSION of a settled status: reporting 'created' or a run
// state as an action item would tell an operator that a healthy paid campaign is stuck,
// and 'created_degraded' genuinely exists upstream (a re-dispatch cannot repair it), so
// none of them belong in a reconciliation queue.
func TestNonSettledCampaignStatusesExcludesSettled(t *testing.T) {
	got := map[string]bool{}
	for _, s := range nonSettledCampaignStatuses() {
		got[s] = true
	}
	for _, want := range []string{model.CampaignStatusPending, "unconfirmed", "group_created"} {
		if !got[want] {
			t.Errorf("status %q must be reported as needing reconciliation", want)
		}
	}
	for _, settled := range []string{
		model.CampaignStatusCreated,
		model.CampaignStatusCreatedDegraded,
		model.CampaignRunActive,
		model.CampaignRunPaused,
	} {
		if got[settled] {
			t.Errorf("settled status %q must NOT be reported as needing reconciliation", settled)
		}
	}
}

// TestPartialOrphanStatusesMatchService guards the literals duplicated from
// internal/service's partialOrphanStatuses. They cannot be imported (that package
// imports this one, so a real import would cycle), so this test is what keeps the two
// from drifting.
//
// Drift here is silent and harmful in BOTH directions: a status this file omits would
// classify a partial orphan as a bare, RELEASABLE claim — the exact mistake that
// authorizes a duplicate paid campaign — while an extra status would report a healthy
// campaign as stuck.
func TestPartialOrphanStatusesMatchService(t *testing.T) {
	// Mirrors internal/service/orchestrator.go's partialOrphanStatuses map.
	want := map[string]bool{"group_created": true, "unconfirmed": true}
	if len(partialOrphanStatuses) != len(want) {
		t.Fatalf("partialOrphanStatuses = %v, want the same %d entries as internal/service", partialOrphanStatuses, len(want))
	}
	for _, s := range partialOrphanStatuses {
		if !want[s] {
			t.Errorf("unexpected partial-orphan status %q; internal/service does not define it", s)
		}
	}
}

// TestReleaseSQLGatesEveryPrecondition asserts the locking read is a FOR UPDATE that
// selects every field the release decision depends on.
//
// This is a structural check, deliberately paired with (not a substitute for) the
// live-database tests: a mock cannot reproduce FOR UPDATE semantics, and a string check
// alone would pass against a query that deletes live claims. Its value is catching the
// specific regression of someone "simplifying" the locking read into a plain SELECT,
// which compiles, passes a naive unit test, and silently reopens the TOCTOU window.
func TestReleaseSQLGatesEveryPrecondition(t *testing.T) {
	for _, frag := range []string{
		"FOR UPDATE",      // serializes against a concurrent writer of this row
		"project_id = $3", // tenant isolation
		"result IS NOT NULL AS has_result",
		"version",
		"platform_campaign_id",
		"created_at",
	} {
		if !strings.Contains(releaseLockQuery, frag) {
			t.Errorf("releaseLockQuery must contain %q; without it the release can act on stale or cross-tenant state", frag)
		}
	}
}
