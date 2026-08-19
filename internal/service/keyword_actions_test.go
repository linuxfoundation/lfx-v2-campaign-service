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
	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// keywordActionDispatcher is a dispatcher implementing ONLY the keyword capabilities, so a
// test can drive the service without the create path. err flips every capability to its
// error arm.
type keywordActionDispatcher struct {
	err error
	// dropOutcomes, when > 0, returns that many FEWER outcomes than requested. It exists to
	// separate applied_count's two possible sources: the request length and the outcomes the
	// platform actually confirmed. On the happy path those are equal, so a handler reporting
	// the request length passes every ordinary test while claiming success for keywords the
	// platform never confirmed.
	dropOutcomes int
	// gotActions records what reached the dispatcher, so a test can assert the service
	// forwarded the caller's batch rather than a silently-altered one.
	gotActions []model.KeywordAction
}

func (d *keywordActionDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *keywordActionDispatcher) ApplyKeywordActions(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, actions []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	d.gotActions = actions
	if d.err != nil {
		return nil, d.err
	}
	kept := actions
	if d.dropOutcomes > 0 && d.dropOutcomes <= len(actions) {
		kept = actions[:len(actions)-d.dropOutcomes]
	}
	out := make([]model.KeywordActionOutcome, 0, len(kept))
	for _, a := range kept {
		out = append(out, model.KeywordActionOutcome{
			AdGroupID:    a.AdGroupID,
			CriterionID:  a.CriterionID,
			Action:       a.Action,
			ResourceName: "customers/1/adGroupCriteria/" + a.AdGroupID + "~" + a.CriterionID,
		})
	}
	return out, nil
}

func (d *keywordActionDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow) (*model.KeywordPerformance, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.KeywordPerformance{Window: model.MetricsWindowLast30Days, Rows: []model.KeywordRow{{CriterionID: "777", AdGroupID: "333"}}}, nil
}

func (d *keywordActionDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow) (*model.AudienceInsights, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.AudienceInsights{Window: model.MetricsWindowLast30Days, Buckets: []model.AudienceBucket{{Dimension: model.AudienceDimensionAge, Value: "AGE_RANGE_25_34"}}}, nil
}

// keywordActionService wires a BriefService whose campaign repo holds one campaign on the
// given platform.
func keywordActionService(t *testing.T, platform model.Provider, d PlatformDispatcher) *BriefService {
	t.Helper()
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{
		"c1": {ID: "c1", Platform: platform, PlatformCampaignID: "555"},
	}}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{platform: d})
	return NewBriefService(repo, camps, jobs, orch)
}

func keywordActionPayload(actions ...*briefs.KeywordActionInput) *briefs.ApplyKeywordActionsPayload {
	return &briefs.ApplyKeywordActionsPayload{
		ProjectID:  "cncf",
		BriefID:    "b1",
		CampaignID: "c1",
		Actions:    actions,
	}
}

func TestApplyKeywordActions_HappyPath(t *testing.T) {
	d := &keywordActionDispatcher{}
	s := keywordActionService(t, model.ProviderGoogleAds, d)

	res, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "888", Action: "REMOVE"},
	))
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if res.CampaignID != "c1" {
		t.Errorf("CampaignID = %q", res.CampaignID)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(res.Results))
	}
	// applied_count must reflect the outcomes actually returned, not the request length —
	// otherwise it could report success for a batch the platform never confirmed.
	if res.AppliedCount != 2 {
		t.Errorf("AppliedCount = %d, want 2", res.AppliedCount)
	}
	if res.Results[0].Action != "PAUSE" || res.Results[1].Action != "REMOVE" {
		t.Errorf("actions not preserved in order: %+v", res.Results)
	}
	// The service must forward the caller's batch unaltered.
	if len(d.gotActions) != 2 || d.gotActions[0].CriterionID != "777" || d.gotActions[1].Action != "REMOVE" {
		t.Errorf("dispatcher received %+v", d.gotActions)
	}
}

// applied_count must be derived from the outcomes the platform CONFIRMED, never from the
// request length. The two are equal on every happy path, so without this test a handler
// reporting len(p.Actions) reports success for keywords that were never confirmed changed.
func TestApplyKeywordActions_AppliedCountFollowsConfirmedOutcomes(t *testing.T) {
	s := keywordActionService(t, model.ProviderGoogleAds, &keywordActionDispatcher{dropOutcomes: 1})

	res, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "888", Action: "REMOVE"},
	))
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(res.Results))
	}
	if res.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1 (it must count CONFIRMED outcomes, not the 2 requested)", res.AppliedCount)
	}
}

// Google Ads is the only platform modelling keywords as addressable criteria. Any other
// campaign must be refused as a permanent caller error, and the platform must not be called.
func TestApplyKeywordActions_NonGoogleAdsCampaignIsBadRequest(t *testing.T) {
	d := &keywordActionDispatcher{}
	s := keywordActionService(t, model.ProviderRedditAds, d)

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
	))
	if err == nil {
		t.Fatal("expected a 400 for a non-Google-Ads campaign, got nil")
	}
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("error = %T (%v), want *briefs.BadRequestError", err, err)
	}
	if d.gotActions != nil {
		t.Error("the dispatcher was called for a non-Google-Ads campaign")
	}
}

// An empty batch must not report success: a 200 would tell the caller their keywords were
// paused when no request was ever made.
func TestApplyKeywordActions_EmptyBatchIsBadRequest(t *testing.T) {
	d := &keywordActionDispatcher{}
	s := keywordActionService(t, model.ProviderGoogleAds, d)

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload())
	if err == nil {
		t.Fatal("expected a 400 for an empty batch, got nil")
	}
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("error = %T (%v), want *briefs.BadRequestError", err, err)
	}
	if d.gotActions != nil {
		t.Error("the dispatcher was called for an empty batch")
	}
}

// Each adapter failure must reach the caller as the status its remedy implies. A 503 for a
// permanent fault invites a retry that can never succeed.
func TestApplyKeywordActions_ErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr any
	}{
		{"invalid batch", domain.ErrKeywordActionInvalid, &briefs.BadRequestError{}},
		{"unsupported platform", domain.ErrKeywordActionsUnsupported, &briefs.BadRequestError{}},
		{"not provisioned", domain.ErrCampaignNotProvisioned, &briefs.ConflictError{}},
		{"account mismatch", domain.ErrCampaignAccountMismatch, &briefs.ConflictError{}},
		{"connection unusable", domain.ErrConnectionNotUsable, &briefs.ConflictError{}},
		{"no connection row", domain.ErrNotFound, &briefs.NotFoundError{}},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, &briefs.InternalServerError{}},
		{"credential decryption failed", domain.ErrCredentialDecryptionFailed, &briefs.InternalServerError{}},
		{"upstream failure", errors.New("boom"), &briefs.ConnServiceUnavailableError{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := keywordActionService(t, model.ProviderGoogleAds, &keywordActionDispatcher{err: tc.err})
			_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
				&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
			))
			if err == nil {
				t.Fatalf("expected an error for %v, got nil", tc.err)
			}
			switch tc.wantErr.(type) {
			case *briefs.BadRequestError:
				if _, ok := err.(*briefs.BadRequestError); !ok {
					t.Fatalf("error = %T (%v), want *briefs.BadRequestError", err, err)
				}
			case *briefs.ConflictError:
				if _, ok := err.(*briefs.ConflictError); !ok {
					t.Fatalf("error = %T (%v), want *briefs.ConflictError", err, err)
				}
			case *briefs.NotFoundError:
				if _, ok := err.(*briefs.NotFoundError); !ok {
					t.Fatalf("error = %T (%v), want *briefs.NotFoundError", err, err)
				}
			case *briefs.InternalServerError:
				if _, ok := err.(*briefs.InternalServerError); !ok {
					t.Fatalf("error = %T (%v), want *briefs.InternalServerError", err, err)
				}
			case *briefs.ConnServiceUnavailableError:
				if _, ok := err.(*briefs.ConnServiceUnavailableError); !ok {
					t.Fatalf("error = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
				}
			}
		})
	}
}

// The adapter's own error text can name ad group ids and embed upstream response bodies. It
// must never be echoed to the caller.
func TestApplyKeywordActions_AdapterTextIsNotEchoed(t *testing.T) {
	secret := "criterion 999 in ad group 424242 of customer 7777777777"
	s := keywordActionService(t, model.ProviderGoogleAds,
		&keywordActionDispatcher{err: errors.Join(domain.ErrCampaignAccountMismatch, errors.New(secret))})

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
	))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	ce, ok := err.(*briefs.ConflictError)
	if !ok {
		t.Fatalf("error = %T", err)
	}
	if strings.Contains(ce.Message, "424242") || strings.Contains(ce.Message, "7777777777") {
		t.Errorf("response echoed adapter detail: %q", ce.Message)
	}
}

// ─── connection-service reads ───

func keywordInsightsService(t *testing.T, d PlatformDispatcher) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	svc.SetOrchestrator(NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: d}))
	return svc
}

func TestGetGoogleAdsKeywords_HappyPath(t *testing.T) {
	svc := keywordInsightsService(t, &keywordActionDispatcher{})
	res, err := svc.GetGoogleAdsKeywords(context.Background(), &conn.GetGoogleAdsKeywordsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetGoogleAdsKeywords: %v", err)
	}
	if len(res.Rows) != 1 || res.RowCount != 1 {
		t.Fatalf("rows = %d, row_count = %d", len(res.Rows), res.RowCount)
	}
	if res.Rows[0].CriterionID != "777" {
		t.Errorf("row = %+v", res.Rows[0])
	}
}

func TestGetGoogleAdsAudience_HappyPath(t *testing.T) {
	svc := keywordInsightsService(t, &keywordActionDispatcher{})
	res, err := svc.GetGoogleAdsAudience(context.Background(), &conn.GetGoogleAdsAudiencePayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetGoogleAdsAudience: %v", err)
	}
	if len(res.Buckets) != 1 || res.BucketCount != 1 {
		t.Fatalf("buckets = %d, bucket_count = %d", len(res.Buckets), res.BucketCount)
	}
	if res.Buckets[0].Dimension != model.AudienceDimensionAge {
		t.Errorf("bucket = %+v", res.Buckets[0])
	}
}

// The reserved LF scope is unaddressable: left open, a GET here would decrypt the LF
// credential and report the Linux Foundation's own keyword performance to any caller.
func TestGetGoogleAdsInsights_RejectsSystemScope(t *testing.T) {
	svc := keywordInsightsService(t, &keywordActionDispatcher{})

	if _, err := svc.GetGoogleAdsKeywords(context.Background(), &conn.GetGoogleAdsKeywordsPayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Fatal("keywords: expected the reserved system scope to be rejected")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("keywords: error = %T (%v), want *conn.NotFoundError", err, err)
	}

	if _, err := svc.GetGoogleAdsAudience(context.Background(), &conn.GetGoogleAdsAudiencePayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Fatal("audience: expected the reserved system scope to be rejected")
	} else if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("audience: error = %T (%v), want *conn.NotFoundError", err, err)
	}
}

// An invalid window is refused locally, before the platform is contacted.
func TestGetGoogleAdsKeywords_InvalidWindowIsBadRequest(t *testing.T) {
	d := &keywordActionDispatcher{}
	svc := keywordInsightsService(t, d)
	bad := "next_tuesday"
	_, err := svc.GetGoogleAdsKeywords(context.Background(), &conn.GetGoogleAdsKeywordsPayload{ProjectID: "cncf", Window: &bad})
	if err == nil {
		t.Fatal("expected a 400 for an invalid window, got nil")
	}
	if _, ok := err.(*conn.BadRequestError); !ok {
		t.Fatalf("error = %T (%v), want *conn.BadRequestError", err, err)
	}
}

func TestGetGoogleAdsKeywords_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want any
	}{
		{"unsupported", domain.ErrKeywordInsightsUnsupported, &conn.BadRequestError{}},
		{"no connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", domain.ErrConnectionNotUsable, &conn.BadRequestError{}},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, &conn.InternalServerError{}},
		{"upstream failure", errors.New("boom"), &conn.ConnServiceUnavailableError{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := keywordInsightsService(t, &keywordActionDispatcher{err: tc.err})
			_, err := svc.GetGoogleAdsKeywords(context.Background(), &conn.GetGoogleAdsKeywordsPayload{ProjectID: "cncf"})
			if err == nil {
				t.Fatalf("expected an error for %v, got nil", tc.err)
			}
			switch tc.want.(type) {
			case *conn.BadRequestError:
				if _, ok := err.(*conn.BadRequestError); !ok {
					t.Fatalf("error = %T (%v), want *conn.BadRequestError", err, err)
				}
			case *conn.NotFoundError:
				if _, ok := err.(*conn.NotFoundError); !ok {
					t.Fatalf("error = %T (%v), want *conn.NotFoundError", err, err)
				}
			case *conn.InternalServerError:
				if _, ok := err.(*conn.InternalServerError); !ok {
					t.Fatalf("error = %T (%v), want *conn.InternalServerError", err, err)
				}
			case *conn.ConnServiceUnavailableError:
				if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
					t.Fatalf("error = %T (%v), want *conn.ConnServiceUnavailableError", err, err)
				}
			}
		})
	}
}
