// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// cs-1: the create input's funnel_stage, tactic and expected_slot_versions are checked before any
// job exists, so no refused request reaches a dispatcher or an ad platform.

func approvedPaidBriefService(t *testing.T) (*BriefService, *fakeJobRepo, *fakeCampaignRepo) {
	t.Helper()
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: "kubecon-na-2026", Status: model.BriefApproved,
		DeliveryType: model.DeliveryPaidMarketing, Version: 1,
	}
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	return NewBriefService(repo, camps, jobs, NewOrchestrator(camps, jobs, nil)), jobs, camps
}

// AC 13: an unlisted stage or tactic — including adversarial text aimed at the pipe-delimited
// campaign name or the data pipeline's name parsers — is a 400 and creates no job.
func TestBriefService_CreateCampaigns_RefusesUnlistedLabelsBeforeAnyJob(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stage, tactic *string
		want          string
	}{
		{"unknown stage", strp("awareness"), nil, "funnel_stage"},
		{"pipe injection in the stage", strp("top | ToFU"), nil, "funnel_stage"},
		{"free-text tactic", nil, strp("retarget video tofu"), "tactic"},
		{"pipe injection in the tactic", nil, strp("keyword | BoFU | x"), "tactic"},
		{"case variant of a listed tactic", nil, strp("Retargeting"), "tactic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, jobs, _ := approvedPaidBriefService(t)
			_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
				ProjectID: "cncf", BriefID: "b1",
				Input: &briefs.CampaignCreateInput{
					Platforms: []string{"microsoft-ads"}, FunnelStage: tc.stage, Tactic: tc.tactic,
				},
			})
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) || !strings.Contains(bad.Message, tc.want) {
				t.Fatalf("got %T (%v), want a 400 naming %s", err, err, tc.want)
			}
			if n := len(jobs.jobs); n != 0 {
				t.Fatalf("%d job(s) created for a refused request", n)
			}
		})
	}
}

// expected_slot_versions is REQUIRED with new_version for every selected platform, refused
// without new_version, and its keys and values are checked — all before a job exists.
func TestBriefService_CreateCampaigns_ExpectedSlotVersionsRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newVer   bool
		expected map[string]int
		want     string
	}{
		{"new_version without it", true, nil, "missing microsoft-ads"},
		{"new_version with an empty map", true, map[string]int{}, "missing microsoft-ads"},
		{"a key that is not a selected platform", true, map[string]int{"microsoft-ads": 1, "google-ads": 1}, "not a selected platform"},
		{"a negative value", true, map[string]int{"microsoft-ads": -1}, "0 or more"},
		{"given without new_version", false, map[string]int{"microsoft-ads": 1}, "applies only with new_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, jobs, _ := approvedPaidBriefService(t)
			_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
				ProjectID: "cncf", BriefID: "b1",
				Input: &briefs.CampaignCreateInput{
					Platforms: []string{"microsoft-ads"}, NewVersion: tc.newVer, ExpectedSlotVersions: tc.expected,
				},
			})
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) || !strings.Contains(bad.Message, tc.want) {
				t.Fatalf("got %T (%v), want a 400 containing %q", err, err, tc.want)
			}
			if n := len(jobs.jobs); n != 0 {
				t.Fatalf("%d job(s) created for a refused request", n)
			}
		})
	}
}

// The validated expectation and labels reach the orchestrator: a well-formed new_version request
// whose expectation is stale (the caller saw no campaign; slot 1 exists) is answered as a reuse,
// which only happens if the service passed the map through.
func TestBriefService_CreateCampaigns_PassesTheExpectationToTheOrchestrator(t *testing.T) {
	s, jobs, camps := approvedPaidBriefService(t)
	camps.storeRow(&model.Campaign{
		ID: "row-1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderMicrosoftAds,
		Variant: model.VariantDefault, SlotVersion: 1, PlatformCampaignID: "ms-1", Status: "created",
	})
	res, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{
			Platforms: []string{"microsoft-ads"}, NewVersion: true,
			ExpectedSlotVersions: map[string]int{"microsoft-ads": 0},
			FunnelStage:          strp("mid"), Tactic: strp("keyword"),
		},
	})
	if err != nil {
		t.Fatalf("CreateCampaigns: %v", err)
	}
	j := waitForTerminal(t, jobs, res.JobID)
	got := decodeResults(t, j)
	if len(got) != 1 || !got[0].Reused || got[0].CampaignRowID != "row-1" {
		t.Fatalf("result = %+v, want row-1 reused: a stale expectation must not create", got)
	}
}

// GetJob surfaces which of the brief's campaigns each platform result is (cs-1), and a job
// recorded before those fields existed still polls with them absent.
func TestBriefService_GetJob_IdentifiesTheCampaignRow(t *testing.T) {
	s := getJobTestService(&model.CampaignJob{
		ID: "j1", BriefID: "b1", Status: model.JobSucceeded,
		Result: []byte(`[` +
			`{"platform":"microsoft-ads","ok":true,"campaign_id":"ms-2","campaign_row_id":"row-2","slot_version":2,"variant":"default","funnel_stage":"mid","tactic":"keyword","reused":true},` +
			`{"platform":"google-ads","ok":true,"campaign_id":"g-1"}]`),
	})
	res, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(res.Result) != 2 {
		t.Fatalf("results = %d, want 2", len(res.Result))
	}
	ms, legacy := res.Result[0], res.Result[1]
	if ms.CampaignRowID == nil || *ms.CampaignRowID != "row-2" || ms.SlotVersion == nil || *ms.SlotVersion != 2 ||
		ms.Variant == nil || *ms.Variant != "default" || ms.FunnelStage == nil || *ms.FunnelStage != "mid" ||
		ms.Tactic == nil || *ms.Tactic != "keyword" || ms.Reused == nil || !*ms.Reused {
		t.Errorf("microsoft result = %+v, want row-2 / slot 2 / default / mid / keyword / reused", ms)
	}
	if legacy.CampaignRowID != nil || legacy.SlotVersion != nil || legacy.FunnelStage != nil || legacy.Reused != nil {
		t.Errorf("a pre-cs-1 result must poll with the new fields absent, got %+v", legacy)
	}
}

// The campaign response carries variant, funnel_stage, tactic and created_at (cs-1); an
// unlabelled row leaves the labels absent rather than "".
func TestCampaignResult_CarriesVariantLabelsAndCreatedAt(t *testing.T) {
	created := time.Date(2026, 11, 2, 18, 4, 5, 0, time.UTC)
	got := campaignResult(&model.Campaign{
		ID: "c1", Platform: model.ProviderGoogleAds, Variant: "demand-gen", SlotVersion: 2, CreatedAt: created,
		FunnelStage: stagePtr(model.FunnelStageTop), Tactic: tacticPtr(model.TacticLookalike),
	})
	if got.Variant == nil || *got.Variant != "demand-gen" || got.FunnelStage == nil || *got.FunnelStage != "top" ||
		got.Tactic == nil || *got.Tactic != "lookalike" || got.CreatedAt == nil || *got.CreatedAt != "2026-11-02T18:04:05Z" {
		t.Errorf("campaign = %+v, want demand-gen / top / lookalike / 2026-11-02T18:04:05Z", got)
	}
	legacy := campaignResult(&model.Campaign{ID: "c0", Platform: model.ProviderMetaAds})
	if legacy.FunnelStage != nil || legacy.Tactic != nil || legacy.CreatedAt != nil {
		t.Errorf("an unlabelled, unread row must leave labels and created_at absent, got %+v", legacy)
	}
	if legacy.Variant == nil || *legacy.Variant != model.VariantDefault {
		t.Errorf("variant = %v, want the normalized default", legacy.Variant)
	}
}

// The indexed campaign document carries the same variant, labels and created_at (cs-1), and an
// unlabelled campaign's document omits the labels.
func TestCampaignDoc_CarriesVariantLabelsAndCreatedAt(t *testing.T) {
	created := time.Date(2026, 11, 2, 18, 4, 5, 0, time.UTC)
	doc := campaignDoc(campaignResult(&model.Campaign{
		ID: "c1", Platform: model.ProviderMicrosoftAds, SlotVersion: 2, CreatedAt: created,
		FunnelStage: stagePtr(model.FunnelStageMid), Tactic: tacticPtr(model.TacticKeyword),
	}))
	if doc.Variant != model.VariantDefault || doc.FunnelStage == nil || *doc.FunnelStage != "mid" ||
		doc.Tactic == nil || *doc.Tactic != "keyword" || doc.CreatedAt == nil || *doc.CreatedAt != "2026-11-02T18:04:05Z" {
		t.Errorf("doc = %+v, want default / mid / keyword / created_at", doc)
	}
	legacy := campaignDoc(campaignResult(&model.Campaign{ID: "c0", Platform: model.ProviderMicrosoftAds}))
	if legacy.FunnelStage != nil || legacy.Tactic != nil {
		t.Errorf("an unlabelled campaign's document must omit the labels, got %+v", legacy)
	}
}
