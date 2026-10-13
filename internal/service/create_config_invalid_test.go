// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// A config-only refusal (LFXV2-2665: imageAssetId with signed asset URLs off) must reach the
// caller as a synchronous refusal, and — since nothing left the process — record no upstream
// call.
func TestOrchestrator_PreflightCreate_ConfigInvalidRefusesUnrecorded(t *testing.T) {
	rec := &recordingMetrics{}
	stub := &preflightStub{err: fmt.Errorf("%w: stored creative assets are not enabled for Reddit; supply imageUrl", ErrCreateConfigInvalid)}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderRedditAds: stub,
	})
	orch.SetMetrics(rec)

	err := orch.PreflightCreate(context.Background(), "proj-1", []model.Provider{model.ProviderRedditAds}, nil)
	if !errors.Is(err, ErrCreateConfigInvalid) {
		t.Fatalf("PreflightCreate = %v, want ErrCreateConfigInvalid", err)
	}
	if got := rec.upstreamCalls(); len(got) != 0 {
		t.Fatalf("a config-only refusal recorded %d upstream calls, want 0", len(got))
	}
}

func TestMapBriefErr_CreateConfigInvalidIs400WithItsMessage(t *testing.T) {
	const msg = "stored creative assets are not enabled for Reddit; supply imageUrl"
	err := mapBriefErr(fmt.Errorf("%w: %s", ErrCreateConfigInvalid, msg))
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("mapped to %T, want *BadRequestError", err)
	}
	if bad.Code != "400" || bad.Message != "invalid campaign config: "+msg {
		t.Fatalf("400 body = %+v", bad)
	}
}
