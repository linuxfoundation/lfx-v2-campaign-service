// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
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

// briefPreflightStub is a dispatcher with only the brief-dependent pre-check.
type briefPreflightStub struct {
	err      error
	gotBrief *model.CampaignBrief
}

func (s *briefPreflightStub) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (s *briefPreflightStub) PreflightCreateBrief(_ context.Context, _ string, _ model.Provider, brief *model.CampaignBrief, _ json.RawMessage) error {
	s.gotBrief = brief
	return s.err
}

// PreflightCreateBrief hands the brief to the dispatcher, refuses on ErrCreateConfigInvalid
// (records no upstream call: the check is local) and is fail-open on anything else.
func TestOrchestrator_PreflightCreateBrief_RefusesOnlyConfigInvalid(t *testing.T) {
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "proj-1"}
	for name, tc := range map[string]struct {
		err    error
		refuse bool
	}{
		"config invalid": {fmt.Errorf("%w: microsoftConfig.adGroups entry 2: path1: too long", ErrCreateConfigInvalid), true},
		"not applicable": {ErrPreflightNotApplicable, false},
		"other error":    {errors.New("boom"), false},
		"clean":          {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingMetrics{}
			stub := &briefPreflightStub{err: tc.err}
			orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: stub})
			orch.SetMetrics(rec)
			err := orch.PreflightCreateBrief(context.Background(), "proj-1", []model.Provider{model.ProviderMicrosoftAds}, brief, nil)
			if errors.Is(err, ErrCreateConfigInvalid) != tc.refuse || (!tc.refuse && err != nil) {
				t.Fatalf("err = %v, want refuse=%v", err, tc.refuse)
			}
			if stub.gotBrief != brief {
				t.Errorf("the brief did not reach the dispatcher")
			}
			if got := rec.upstreamCalls(); len(got) != 0 {
				t.Errorf("a local check recorded %d upstream calls", len(got))
			}
		})
	}
}
