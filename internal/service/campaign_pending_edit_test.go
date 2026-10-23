// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestUpdateCampaign_ConfigEditOnAPendingCampaignIsRefused pins update-campaign's refusal of a
// CONFIG edit while the campaign is still 'pending' — its create has been claimed but the
// platform call has not resolved.
//
// Three things are asserted, and each of them is the point of a separate part of the guard:
//
//   - It is a 409, not the 400 the status-mismatch check above it returns. The payload re-sends
//     'pending' unchanged, so that check passes and this one is what fires. A 400 here would
//     mean the test was not reaching the guard at all.
//   - The reason slug is the literal `campaign_dispatch_in_flight`. The slug is enforced by the
//     GENERATED CLIENT decoder against the Enum in design/connection.go, so a reason that drifts
//     from that list is not an undocumented value — it is a 409 no generated client can read.
//     Pinning the literal here is what keeps the three copies (design Enum, service constant,
//     wire value) from drifting apart silently.
//   - The refusal happened BEFORE the claim. Validating first keeps an invalid request from
//     taking the campaign's write lock, which acquires a dedicated pooled connection and blocks
//     every other writer for that campaign. camps.claims is the only way to observe this: the
//     claim does not bump the version, so "was it claimed?" cannot be inferred from the row.
func TestUpdateCampaign_ConfigEditOnAPendingCampaignIsRefused(t *testing.T) {
	s, camps := newConfigEditService(json.RawMessage(`{"before":true}`))
	camps.cur.Status = model.CampaignStatusPending

	v := "5"
	_, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{
			CampaignName: "old",
			Status:       model.CampaignStatusPending,
			Config:       map[string]any{"post_url": "https://events.example.org"},
		},
	})
	if !isConflict(err) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	var ce *briefs.ConflictError
	if !errors.As(err, &ce) || ce.Reason == nil {
		t.Fatalf("conflict carries no reason: %#v", err)
	}
	if *ce.Reason != "campaign_dispatch_in_flight" {
		t.Errorf("reason = %q, want %q", *ce.Reason, "campaign_dispatch_in_flight")
	}
	if camps.claims != 0 {
		t.Errorf("a refused edit reached the claim (%d claims)", camps.claims)
	}
	if camps.got != nil {
		t.Errorf("a refused edit reached ReplaceCampaign: %#v", camps.got)
	}
}

// The guard is config-GATED, not status-gated: a name-only edit does not touch config_snapshot,
// so it stays allowed while the campaign is pending and a caller can still correct a typo on a
// campaign that is mid-dispatch. Without this case the guard could be tightened to "no edits
// while pending" and nothing would go red.
func TestUpdateCampaign_NameOnlyEditOnAPendingCampaignSucceeds(t *testing.T) {
	orig := json.RawMessage(`{"before":true}`)
	s, camps := newConfigEditService(orig)
	camps.cur.Status = model.CampaignStatusPending

	v := "5"
	if _, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "renamed", Status: model.CampaignStatusPending},
	}); err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if camps.got == nil {
		t.Fatal("ReplaceCampaign was not called")
	}
	if camps.got.CampaignName != "renamed" {
		t.Errorf("campaign_name = %q, want %q", camps.got.CampaignName, "renamed")
	}
	// ReplaceCampaign writes every mutable column, snapshot included, so the carried-over
	// snapshot is what keeps a name edit from blanking it.
	if string(camps.got.ConfigSnapshot) != string(orig) {
		t.Errorf("snapshot = %s, want unchanged %s", camps.got.ConfigSnapshot, orig)
	}
}
