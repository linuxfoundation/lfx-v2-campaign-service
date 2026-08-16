// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// TestBriefService_UploadCreativeAsset_NotYetImplemented pins the C1 contract-only
// stub: the method exists on the service interface but reports 503 rather than
// pretending to store anything. It asserts the exact typed error, code and message
// so the deferred handler is a visible, tested contract and not a silent no-op. This
// test is REPLACED by the real validate-and-persist handler tests when that lands.
func TestBriefService_UploadCreativeAsset_NotYetImplemented(t *testing.T) {
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
	if svcErr.Message != "creative asset upload is not yet available" {
		t.Errorf("message = %q, want %q", svcErr.Message, "creative asset upload is not yet available")
	}
}
