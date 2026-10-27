// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestRefuseUnsupportedBidding pins LFXV2-2665's Google rule: googleAdsConfig has no `bidding`
// object (its bidding is googleAdsConfig.biddingStrategy), so one is refused rather than silently
// ignored — and only when google-ads is SELECTED, and only the key's presence matters.
func TestRefuseUnsupportedBidding(t *testing.T) {
	google := []model.Provider{model.ProviderGoogleAds}
	cases := []struct {
		name      string
		platforms []model.Provider
		config    string
		refused   bool
	}{
		{"manual bidding on google", google, `{"googleAdsConfig":{"bidding":{"strategy":"manual","bidAmount":1}}}`, true},
		{"automatic bidding on google", google, `{"googleAdsConfig":{"bidding":{"strategy":"automatic"}}}`, true},
		{"null bidding on google is still the key", google, `{"googleAdsConfig":{"bidding":null}}`, true},
		{"google without bidding", google, `{"googleAdsConfig":{"biddingStrategy":"MAXIMIZE_CLICKS"}}`, false},
		{"google not selected", []model.Provider{model.ProviderMetaAds}, `{"googleAdsConfig":{"bidding":{}},"metaConfig":{"bidding":{"strategy":"manual","bidAmount":1}}}`, false},
		{"lane platform bidding is the dispatcher's", []model.Provider{model.ProviderMicrosoftAds}, `{"microsoftConfig":{"bidding":{"strategy":"manual","bidAmount":1}}}`, false},
		{"no config", google, ``, false},
		{"undecodable config is left to dispatch", google, `[1,2]`, false},
		{"undecodable google config is left to dispatch", google, `{"googleAdsConfig":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseUnsupportedBidding(tc.platforms, json.RawMessage(tc.config))
			if !tc.refused {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) || !strings.Contains(bad.Message, "googleAdsConfig.bidding is not supported") {
				t.Fatalf("err = %v, want a 400 naming googleAdsConfig.bidding", err)
			}
		})
	}
}

// TestBriefService_CreateCampaigns_RefusesGoogleBiddingBeforeAnyJob drives the refusal through
// CreateCampaigns: the 400 arrives synchronously, before a job exists.
func TestBriefService_CreateCampaigns_RefusesGoogleBiddingBeforeAnyJob(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: "kubecon-eu-2026",
		DeliveryType: model.DeliveryPaidMarketing, Status: model.BriefApproved, Version: 1,
	}
	s := newTestBriefService(repo)
	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{
			Platforms: []string{"google-ads"},
			Config:    map[string]any{"googleAdsConfig": map[string]any{"bidding": map[string]any{"strategy": "manual", "bidAmount": 2}}},
		},
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "googleAdsConfig.bidding") {
		t.Fatalf("CreateCampaigns = %v, want a 400 naming googleAdsConfig.bidding", err)
	}
}

// TestCampaignResult_ReportsBidding: the recorded strategy and bid reach the API response, and a
// row that recorded neither leaves both absent (never "automatic", never 0).
func TestCampaignResult_ReportsBidding(t *testing.T) {
	manual, bid := model.BiddingManual, 1.25
	got := campaignResult(&model.Campaign{ID: "c1", BiddingStrategy: &manual, MaxCPCBid: &bid})
	if got.BiddingStrategy == nil || *got.BiddingStrategy != "manual" || got.MaxCpcBid == nil || *got.MaxCpcBid != 1.25 {
		t.Fatalf("campaignResult = strategy %v bid %v, want manual / 1.25", got.BiddingStrategy, got.MaxCpcBid)
	}
	legacy := campaignResult(&model.Campaign{ID: "c2"})
	if legacy.BiddingStrategy != nil || legacy.MaxCpcBid != nil {
		t.Fatalf("an unrecorded row must leave both absent, got %v / %v", legacy.BiddingStrategy, legacy.MaxCpcBid)
	}
}
