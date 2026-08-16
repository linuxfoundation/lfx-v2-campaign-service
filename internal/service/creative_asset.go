// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// UploadCreativeAsset stores an uploaded image asset for a brief so a Meta ad
// creative can later reference it by id.
//
// C1 contract-only stub: the Goa contract and generated interface land in this
// commit, but the validate-and-persist handler (MIME sniff, size/dimension checks,
// repository insert, idempotent dedupe) is implemented in a later commit. Until
// then the method reports 503 rather than pretending to store anything, keeping the
// build green without a partial/silent implementation.
func (s *BriefService) UploadCreativeAsset(_ context.Context, _ *briefs.UploadCreativeAssetPayload) (*briefs.CreativeAsset, error) {
	return nil, &briefs.ConnServiceUnavailableError{
		Code:    "503",
		Message: "creative asset upload is not yet available",
	}
}
