// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestUpdateCampaign_ConfigNamingAMissingAssetIsRefused pins the wire half of the
// dangling-creative-asset refusal.
//
// The decision itself cannot be made out here. It belongs inside ReplaceCampaign's transaction,
// under the brief row lock a concurrent DeleteAsset has to take as well — that lock is the only
// thing that makes the answer still true a moment after it is given, and a fake repository has
// no lock to stand under. So the repo is driven to return the sentinel and what this test owns
// is everything downstream of it:
//
//   - It is a 409, not a 400. The configuration the caller sent was well-formed and was valid
//     when they read the campaign; what changed is the world, and a retry after re-uploading
//     the asset is the remedy. That is the 409 contract, and it is why this does not reuse
//     ErrCreateConfigInvalid, whose contract is a create-time 400.
//   - The reason slug is the literal `creative_asset_reference_missing`. The slug is enforced
//     by the GENERATED CLIENT decoder against the Enum in design/connection.go, so a drifted
//     value is not an undocumented reason — it is a 409 no generated client can read. This test
//     calls UpdateCampaign directly and never crosses the generated encoder, so it pins only
//     the SERVICE half: the constant against the value the service returns. Remove the member
//     from design/connection.go, re-run make apigen, and this stays green while every client
//     breaks. The three are kept in step by hand; the constant's own comment says so.
//   - The message names the ids that did not resolve, and tells the caller what to do. Naming
//     them is safe precisely because the caller supplied them in this very request and the repo
//     canonicalized them with uuid.Parse — no platform text, nothing from another tenant.
func TestUpdateCampaign_ConfigNamingAMissingAssetIsRefused(t *testing.T) {
	const missing = "11111111-1111-4111-8111-111111111111"
	s, camps := newConfigEditService(json.RawMessage(`{"before":true}`))
	camps.replaceErr = fmt.Errorf("%w: %s", domain.ErrCreativeAssetReferenceMissing, missing)

	v := "5"
	_, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{
			CampaignName: "old",
			Status:       "created",
			Config:       map[string]any{"imageAssetId": missing},
		},
	})
	if !isConflict(err) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	var ce *briefs.ConflictError
	if !errors.As(err, &ce) || ce.Reason == nil {
		t.Fatalf("conflict carries no reason: %#v", err)
	}
	if *ce.Reason != "creative_asset_reference_missing" {
		t.Errorf("reason = %q, want %q", *ce.Reason, "creative_asset_reference_missing")
	}
	if !strings.Contains(ce.Message, missing) {
		t.Errorf("message does not name the id that failed to resolve: %q", ce.Message)
	}
	if !strings.Contains(ce.Message, "upload the asset again") {
		t.Errorf("message does not tell the caller how to recover: %q", ce.Message)
	}
}

// TestUpdateCampaign_AConfigEditThatResolvesStillSucceeds is the other side of the same guard,
// and it is not a formality: a refusal that fired on every config edit would also satisfy the
// test above. This is the case that fails if the repo arm is ever wired to reject
// unconditionally, or if mapBriefErr's new arm is placed where it swallows a nil error.
func TestUpdateCampaign_AConfigEditThatResolvesStillSucceeds(t *testing.T) {
	s, camps := newConfigEditService(json.RawMessage(`{"before":true}`))

	v := "5"
	if _, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{
			CampaignName: "old",
			Status:       "created",
			Config:       map[string]any{"imageAssetId": "11111111-1111-4111-8111-111111111111"},
		},
	}); err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if camps.got == nil {
		t.Fatal("ReplaceCampaign was not called")
	}
}
