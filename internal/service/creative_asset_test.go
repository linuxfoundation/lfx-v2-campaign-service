// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// TestBriefService_UploadCreativeAsset_UnavailableWithoutRepo pins that a BriefService with no
// creative-asset repository bound — the no-database and cold-start-pending modes — reports 503
// rather than nil-panicking, mirroring how GenerateEmailCopy / FetchEventURL check their own
// late-bound collaborators. The wording is availability-neutral (not "not configured") because a
// cold start has the database configured but the repo not yet bound.
//
// This runs BEFORE the image sniff: an unwired service must 503 regardless of what bytes arrive,
// so the payload here is a valid PNG header precisely to show the repo check short-circuits ahead
// of validation. The full validate-and-persist matrix (sniff, declared/sniffed match, checksum,
// idempotent dedupe, parent-brief gate) is exercised against a fake/real repo in C7.
func TestBriefService_UploadCreativeAsset_UnavailableWithoutRepo(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)

	_, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
		ProjectID:   "cncf",
		BriefID:     "b1",
		ContentType: "image/png",
		Bytes:       []byte{0x89, 0x50, 0x4e, 0x47}, // PNG magic, enough to look like an upload
	})

	var svcErr *briefs.ConnServiceUnavailableError
	if !errors.As(err, &svcErr) {
		t.Fatalf("UploadCreativeAsset: err = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
	}
	if svcErr.Code != "503" {
		t.Errorf("code = %q, want %q", svcErr.Code, "503")
	}
	if svcErr.Message != "creative asset upload is unavailable" {
		t.Errorf("message = %q, want %q", svcErr.Message, "creative asset upload is unavailable")
	}
}
