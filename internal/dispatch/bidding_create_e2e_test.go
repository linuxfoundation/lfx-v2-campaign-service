// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// e2eBriefRepo answers GetBrief with one approved paid brief. Every other method is the embedded
// nil interface: a call to one panics, which is how this test would notice a refusal that reached
// further than it should.
type e2eBriefRepo struct {
	domain.BriefRepository
	brief *model.CampaignBrief
}

func (r e2eBriefRepo) GetBrief(_ context.Context, _, _ string) (*model.CampaignBrief, error) {
	return r.brief, nil
}

// e2eJobRepo counts job creation attempts; a synchronous refusal must leave it at zero.
type e2eJobRepo struct {
	domain.JobRepository
	mu      sync.Mutex
	creates int
}

func (r *e2eJobRepo) CreateJob(context.Context, string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates++
	return nil, errors.New("e2e: a refused create must not create a job")
}

func (r *e2eJobRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.creates
}

// TestCreateCampaigns_LanePlatformBiddingRefusalIsSynchronous400 drives the REAL Meta and Microsoft
// dispatchers through the service: an invalid `bidding` is refused by the dispatcher's
// PreflightCreate and comes back from CreateCampaigns as a synchronous 400 naming the field, with
// no job created and no platform contacted (the dispatchers have no credentials to contact one).
func TestCreateCampaigns_LanePlatformBiddingRefusalIsSynchronous400(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		config   map[string]any
		want     string
	}{
		{"meta manual on a non-click objective", "meta-ads",
			map[string]any{"metaConfig": map[string]any{"objective": "awareness", "bidding": map[string]any{"strategy": "manual", "bidAmount": 1}}},
			"invalid campaign config: metaConfig.bidding: manual bidding is a cost-per-link-click bid cap"},
		{"microsoft automatic with cpcBid", "microsoft-ads",
			map[string]any{"microsoftConfig": map[string]any{"cpcBid": 1.5, "bidding": map[string]any{"strategy": "automatic"}}},
			"invalid campaign config: microsoftConfig.bidding: cpcBid sets a manual ad group bid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf", EventSlug: "kubecon", Status: model.BriefApproved,
				DeliveryType: model.DeliveryPaidMarketing, Version: 1}
			jobs := &e2eJobRepo{}
			orch := service.NewOrchestrator(nil, jobs, map[model.Provider]service.PlatformDispatcher{
				model.ProviderMetaAds:      &MetaDispatcher{},
				model.ProviderMicrosoftAds: &MicrosoftDispatcher{},
			})
			var campaigns struct{ domain.CampaignRepository }
			s := service.NewBriefService(e2eBriefRepo{brief: brief}, campaigns, jobs, orch)
			_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
				ProjectID: "cncf", BriefID: "b1",
				Input: &briefs.CampaignCreateInput{Platforms: []string{tc.platform}, Config: tc.config},
			})
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) || bad.Code != "400" || !strings.HasPrefix(bad.Message, tc.want) {
				t.Fatalf("CreateCampaigns = %v, want a 400 starting %q", err, tc.want)
			}
			if n := jobs.count(); n != 0 {
				t.Fatalf("%d job creation attempts for a refused create, want 0", n)
			}
		})
	}
}

// TestCreateCampaigns_WrongTypedBiddingIsSynchronous400OnEveryLanePlatform (PR #332 review): a
// `bidAmount` sent as a string reaches the caller as the synchronous 400 through CreateCampaigns on
// every lane platform — never a 202 and a failed job — and no job is created.
func TestCreateCampaigns_WrongTypedBiddingIsSynchronous400OnEveryLanePlatform(t *testing.T) {
	lanes := []struct{ platform, key string }{
		{"microsoft-ads", "microsoftConfig"}, {"meta-ads", "metaConfig"}, {"reddit-ads", "redditConfig"},
		{"twitter-ads", "twitterConfig"}, {"linkedin-ads", "linkedInConfig"},
	}
	for _, lane := range lanes {
		t.Run(lane.platform, func(t *testing.T) {
			brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf", EventSlug: "kubecon", Status: model.BriefApproved,
				DeliveryType: model.DeliveryPaidMarketing, Version: 1}
			jobs := &e2eJobRepo{}
			orch := service.NewOrchestrator(nil, jobs, map[model.Provider]service.PlatformDispatcher{
				model.ProviderMicrosoftAds: &MicrosoftDispatcher{},
				model.ProviderMetaAds:      &MetaDispatcher{},
				model.ProviderRedditAds:    &RedditDispatcher{},
				model.ProviderTwitterAds:   &TwitterDispatcher{},
				model.ProviderLinkedInAds:  &LinkedInDispatcher{},
			})
			var campaigns struct{ domain.CampaignRepository }
			s := service.NewBriefService(e2eBriefRepo{brief: brief}, campaigns, jobs, orch)
			_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
				ProjectID: "cncf", BriefID: "b1",
				Input: &briefs.CampaignCreateInput{
					Platforms: []string{lane.platform},
					Config:    map[string]any{lane.key: map[string]any{"bidding": map[string]any{"strategy": "manual", "bidAmount": "1.25"}}},
				},
			})
			want := "invalid campaign config: " + lane.key + ".bidding: bidAmount must be a number"
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) || bad.Code != "400" || !strings.HasPrefix(bad.Message, want) {
				t.Fatalf("CreateCampaigns = %v, want a 400 starting %q", err, want)
			}
			if n := jobs.count(); n != 0 {
				t.Fatalf("%d job creation attempts for a refused create, want 0", n)
			}
		})
	}
}
