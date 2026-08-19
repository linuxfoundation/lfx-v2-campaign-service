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
	// rewriteCriterion, when set, is reported as every outcome's criterion id regardless of
	// what was requested. It separates "the handler rendered the platform's outcomes" from
	// "the handler echoed the request", which are identical on every ordinary path.
	rewriteCriterion string
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
		criterionID := a.CriterionID
		if d.rewriteCriterion != "" {
			criterionID = d.rewriteCriterion
		}
		out = append(out, model.KeywordActionOutcome{
			AdGroupID:    a.AdGroupID,
			CriterionID:  criterionID,
			Action:       a.Action,
			ResourceName: "customers/1/adGroupCriteria/" + a.AdGroupID + "~" + a.CriterionID,
		})
	}
	return out, nil
}

func (d *keywordActionDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []string) (*model.KeywordPerformance, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.KeywordPerformance{Window: model.MetricsWindowLast30Days, Rows: []model.KeywordRow{{CriterionID: "777", AdGroupID: "333"}}}, nil
}

func (d *keywordActionDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []string) (*model.AudienceInsights, error) {
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

// A short outcome slice is a dispatcher contract violation, NOT a partial success. The batch
// is atomic upstream, so "1 of your 2 keywords was paused" is not a representable answer — and
// returning it as a 200 leaves a caller who was stopping a budget leak unable to tell which
// keyword still spends. The orchestrator must refuse it before a handler can render it.
//
// This is the arm the published contract's "applied_count always equals the number requested"
// rests on: it holds because a short result is REFUSED, not because the handler echoes the
// request length.
func TestApplyKeywordActions_ShortOutcomeSliceIsRefusedNotPartialSuccess(t *testing.T) {
	s := keywordActionService(t, model.ProviderGoogleAds, &keywordActionDispatcher{dropOutcomes: 1})

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "888", Action: "REMOVE"},
	))
	if err == nil {
		t.Fatal("a 1-outcome result for a 2-action batch must be refused, never reported as a partial success")
	}
	// It is an upstream contract violation, not a caller error: the batch was valid.
	if _, ok := err.(*briefs.ConnServiceUnavailableError); !ok {
		t.Fatalf("error = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
	}
}

// applied_count must be DERIVED from the outcomes, never echoed from the request length. With
// the count guard above in place the two are always equal by the time a handler runs, so this
// pins the derivation directly at the handler: a dispatcher returning outcomes whose ids differ
// from the request proves which slice was rendered.
func TestApplyKeywordActions_ResultsAreRenderedFromOutcomesNotTheRequest(t *testing.T) {
	d := &keywordActionDispatcher{rewriteCriterion: "999"}
	s := keywordActionService(t, model.ProviderGoogleAds, d)

	res, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
	))
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if res.AppliedCount != 1 {
		t.Fatalf("AppliedCount = %d, want 1", res.AppliedCount)
	}
	// The dispatcher reported criterion 999; the request named 777. A handler echoing the
	// request would render 777 here.
	if res.Results[0].CriterionID != "999" {
		t.Fatalf("CriterionID = %q, want %q — results must be rendered from the platform outcomes, not the request", res.Results[0].CriterionID, "999")
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
		// Must map to its OWN message: this row names no account to reconnect to, so the
		// mismatch arm's "reconnect the original account" is an instruction the operator
		// cannot follow. Re-dispatch is the only remedy.
		{"provenance unknown", domain.ErrCampaignProvenanceUnknown, &briefs.ConflictError{}},
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
	// A non-empty scope, because these tests exercise what happens AFTER the read reaches the
	// platform. An empty scope is answered without contacting the dispatcher at all, which is
	// its own behaviour and has its own tests.
	camps := &fakeCampaignRepo{scopeIDs: []string{"555"}}
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
		// This arm exists so an unsupported window answers 400 rather than falling through to
		// classifyDiscoveryError's 503 default — telling a caller to retry a request that can
		// never succeed. Reachable only for non-HTTP callers, since the design Enum stops it
		// at the decoder, which is exactly why nothing else pins it.
		{"window unsupported", domain.ErrMetricsWindowUnsupported, &conn.BadRequestError{}},
		{"no connection", domain.ErrNotFound, &conn.NotFoundError{}},
		{"connection unusable", domain.ErrConnectionNotUsable, &conn.BadRequestError{}},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, &conn.InternalServerError{}},
		{"upstream failure", errors.New("boom"), &conn.ConnServiceUnavailableError{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := keywordInsightsService(t, &keywordActionDispatcher{err: tc.err})
			// Driven through BOTH readers. They share classifyInsightsError today, but they are
			// separate call sites: without this, the audience path's entire classification
			// could be deleted and the suite would stay green.
			for _, read := range []struct {
				name string
				call func() error
			}{
				{"keywords", func() error {
					_, e := svc.GetGoogleAdsKeywords(context.Background(), &conn.GetGoogleAdsKeywordsPayload{ProjectID: "cncf"})
					return e
				}},
				{"audience", func() error {
					_, e := svc.GetGoogleAdsAudience(context.Background(), &conn.GetGoogleAdsAudiencePayload{ProjectID: "cncf"})
					return e
				}},
			} {
				t.Run(read.name, func(t *testing.T) {
					assertInsightsErr(t, read.call(), tc.want, tc.err)
				})
			}
		})
	}
}

// assertInsightsErr checks that err is the concrete generated type `want` names.
func assertInsightsErr(t *testing.T, err error, want any, src error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error for %v, got nil", src)
	}
	switch want.(type) {
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
}

// The provenance arm must produce a DIFFERENT remedy from the mismatch arm. Both are 409s, so
// only the message distinguishes them — and telling an operator to reconnect an account that
// was never recorded sends them after something that does not exist.
func TestApplyKeywordActions_ProvenanceUnknownSaysRedispatchNotReconnect(t *testing.T) {
	s := keywordActionService(t, model.ProviderGoogleAds,
		&keywordActionDispatcher{err: errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch)})

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
	))
	ce, ok := err.(*briefs.ConflictError)
	if !ok {
		t.Fatalf("error = %T (%v), want *briefs.ConflictError", err, err)
	}
	if !strings.Contains(ce.Message, "re-dispatched") {
		t.Errorf("message does not name the only available remedy: %q", ce.Message)
	}
	// The error also carries ErrCampaignAccountMismatch (joined), so a broad match would win
	// and hand back "reconnect the original account" — which is why the provenance arm must
	// sit ABOVE the mismatch arm.
	if strings.Contains(ce.Message, "reconnect") {
		t.Errorf("message tells the operator to reconnect an account that was never recorded: %q", ce.Message)
	}
}

// nilResultDispatcher returns (nil, nil) — a contract violation the orchestrator must convert
// into an error rather than pass to a handler that dereferences it unconditionally.
type nilResultDispatcher struct{}

func (d nilResultDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d nilResultDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []string) (*model.KeywordPerformance, error) {
	return nil, nil
}

func (d nilResultDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []string) (*model.AudienceInsights, error) {
	return nil, nil
}

func (d nilResultDispatcher) ApplyKeywordActions(context.Context, string, model.Provider, *model.Campaign, []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	return nil, nil
}

// nilSliceDispatcher returns non-nil results carrying NIL slices, so the orchestrator's
// normalisation is the only thing standing between the caller and a `null` on the wire.
type nilSliceDispatcher struct{}

func (d nilSliceDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d nilSliceDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []string) (*model.KeywordPerformance, error) {
	return &model.KeywordPerformance{Window: model.MetricsWindowLast30Days}, nil
}

func (d nilSliceDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []string) (*model.AudienceInsights, error) {
	return &model.AudienceInsights{Window: model.MetricsWindowLast30Days}, nil
}

// scopeIDs is non-empty: these tests assert what the orchestrator does with the dispatcher's
// RESULT, and an empty scope never reaches the dispatcher.
func keywordOrchestrator(d PlatformDispatcher) *Orchestrator {
	return NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"555"}}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: d,
	})
}

// (nil, nil) is a contract violation, not success: the handler dereferences the result
// unconditionally on a nil error, so passing it through is a panicked request.
func TestOrchestratorKeywordReads_NilResultIsAnError(t *testing.T) {
	orch := keywordOrchestrator(nilResultDispatcher{})
	ctx := context.Background()

	if _, err := orch.ReadKeywordPerformance(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("ReadKeywordPerformance: a nil result with no error must be rejected")
	}
	if _, err := orch.ReadAudienceInsights(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("ReadAudienceInsights: a nil result with no error must be rejected")
	}
	// On the MUTATION the stakes are higher: applied_count is derived from the slice length,
	// so a nil normalised to empty would report "zero keywords changed" for a call that
	// returned success — the exact ambiguity the all-or-nothing batch exists to remove.
	campaign := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "555"}
	if _, err := orch.ApplyKeywordActions(ctx, "p1", model.ProviderGoogleAds, campaign,
		[]model.KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: model.KeywordActionPause}}); err == nil {
		t.Error("ApplyKeywordActions: no outcomes with no error must be rejected, never reported as zero applied")
	}
}

// A successful read must carry a non-nil slice so it serializes as `[]`, never `null` — the
// caller cannot otherwise tell "this account authoritatively has none" from a fall-through.
func TestOrchestratorKeywordReads_NilSlicesAreNormalised(t *testing.T) {
	orch := keywordOrchestrator(nilSliceDispatcher{})
	ctx := context.Background()

	kp, err := orch.ReadKeywordPerformance(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadKeywordPerformance: %v", err)
	}
	if kp.Rows == nil {
		t.Error("Rows is nil; it must be an empty slice so the wire shape is [] not null")
	}

	ai, err := orch.ReadAudienceInsights(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadAudienceInsights: %v", err)
	}
	if ai.Buckets == nil {
		t.Error("Buckets is nil; it must be an empty slice so the wire shape is [] not null")
	}
}

// The orchestrator's own pre-platform guard: never contact the ad platform for a campaign
// with nothing provisioned upstream. Independent of any one adapter's re-check.
func TestOrchestratorApplyKeywordActions_UnprovisionedIsRefused(t *testing.T) {
	orch := keywordOrchestrator(&keywordActionDispatcher{})
	actions := []model.KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: model.KeywordActionPause}}

	for _, tc := range []struct {
		name     string
		campaign *model.Campaign
	}{
		{"nil campaign", nil},
		{"empty platform campaign id", &model.Campaign{Platform: model.ProviderGoogleAds}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := orch.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, tc.campaign, actions)
			if !errors.Is(err, ErrCampaignNotProvisioned) {
				t.Fatalf("error = %v, want ErrCampaignNotProvisioned", err)
			}
		})
	}
}

// A platform with no keyword capability wired must answer a clean "not supported", never a
// transient failure the caller would retry forever.
func TestOrchestratorKeywords_UnsupportedPlatform(t *testing.T) {
	// A dispatcher implementing ONLY Dispatch — neither keyword capability.
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderRedditAds: upstreamCapableDispatcher{},
	})
	ctx := context.Background()

	// An unregistered platform.
	if _, err := orch.ReadKeywordPerformance(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("unregistered platform: error = %v, want ErrKeywordInsightsUnsupported", err)
	}
	campaign := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "555"}
	if _, err := orch.ApplyKeywordActions(ctx, "p1", model.ProviderGoogleAds, campaign, []model.KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: model.KeywordActionPause}}); !errors.Is(err, domain.ErrKeywordActionsUnsupported) {
		t.Errorf("unregistered platform: error = %v, want ErrKeywordActionsUnsupported", err)
	}
}

// countingInsightsDispatcher records whether the platform was contacted at all. The COUNT is the
// assertion: "returned an empty result" and "never issued the query" are different facts, and only
// the second one closes the exposure.
type countingInsightsDispatcher struct {
	keywordCalls  int
	audienceCalls int
	gotIDs        []string
}

func (d *countingInsightsDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("not used")
}

func (d *countingInsightsDispatcher) ReadKeywordPerformance(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, ids []string) (*model.KeywordPerformance, error) {
	d.keywordCalls++
	d.gotIDs = ids
	return &model.KeywordPerformance{Window: w, Rows: []model.KeywordRow{{CriterionID: "leaked-from-another-project"}}}, nil
}

func (d *countingInsightsDispatcher) ReadAudienceInsights(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, ids []string) (*model.AudienceInsights, error) {
	d.audienceCalls++
	d.gotIDs = ids
	return &model.AudienceInsights{Window: w, Buckets: []model.AudienceBucket{{Dimension: "age_range", Value: "leaked"}}}, nil
}

// TestKeywordInsights_EmptyScopeIssuesNoUpstreamCall is the test that makes project-scoping real.
//
// Scoping the GAQL to the project's campaigns closes the cross-project read ONLY if the empty case
// is handled before a query is built. An empty id list rendered into the predicate produces either
// `campaign.id IN ()` or, if the predicate is dropped as "nothing to filter", the original
// account-wide read — handing every other project's keywords and demographics to precisely the
// caller that owns no campaigns.
//
// It asserts the CALL COUNT, not merely that the result was empty. A dispatcher that is contacted
// and whose rows are then discarded would satisfy an empty-result assertion while still having
// issued the exposing query upstream; the rows returned here are deliberately non-empty so that
// any implementation which calls through fails loudly instead of coincidentally looking correct.
func TestKeywordInsights_EmptyScopeIssuesNoUpstreamCall(t *testing.T) {
	d := &countingInsightsDispatcher{}
	// scopeIDs is explicitly empty: the project owns no campaigns on this platform.
	orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: d})
	ctx := context.Background()

	kp, err := orch.ReadKeywordPerformance(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadKeywordPerformance: an empty scope is an ordinary state, not an error: %v", err)
	}
	if d.keywordCalls != 0 {
		t.Errorf("keyword read contacted the platform %d time(s) with an EMPTY scope; the query "+
			"would be unscoped and return every project's keywords from the shared customer",
			d.keywordCalls)
	}
	if kp == nil || len(kp.Rows) != 0 {
		t.Errorf("rows = %+v, want an empty result for a project that owns no campaigns", kp)
	}
	if kp != nil && kp.Rows == nil {
		t.Error("Rows is nil rather than an empty slice: a consumer cannot tell " +
			"'no keywords' from 'the field was never populated'")
	}

	ai, err := orch.ReadAudienceInsights(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadAudienceInsights: %v", err)
	}
	if d.audienceCalls != 0 {
		t.Errorf("audience read contacted the platform %d time(s) with an EMPTY scope; the "+
			"demographic queries would aggregate every project's traffic", d.audienceCalls)
	}
	if ai == nil || len(ai.Buckets) != 0 {
		t.Errorf("buckets = %+v, want an empty result", ai)
	}
	if ai != nil && ai.Buckets == nil {
		t.Error("Buckets is nil rather than an empty slice")
	}
}

// TestKeywordInsights_ScopeIsPassedToTheAdapter pins that the ids actually reach the adapter.
// Without this, the empty-scope guard could pass while a non-empty scope was silently dropped —
// leaving the account-wide read in place for every project that DOES own campaigns.
func TestKeywordInsights_ScopeIsPassedToTheAdapter(t *testing.T) {
	d := &countingInsightsDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"111", "222"}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: d})

	if _, err := orch.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); err != nil {
		t.Fatalf("ReadKeywordPerformance: %v", err)
	}
	if d.keywordCalls != 1 {
		t.Fatalf("keyword calls = %d, want 1", d.keywordCalls)
	}
	if len(d.gotIDs) != 2 || d.gotIDs[0] != "111" || d.gotIDs[1] != "222" {
		t.Errorf("adapter received scope %v, want the project's own campaign ids; a dropped "+
			"scope restores the account-wide read", d.gotIDs)
	}
}

// TestKeywordInsights_ScopeLookupFailureDoesNotFallBack pins that a repo failure is an ERROR
// rather than a silent widening. "We could not determine which campaigns you own" must never
// degrade into "so here is everything".
func TestKeywordInsights_ScopeLookupFailureDoesNotFallBack(t *testing.T) {
	d := &countingInsightsDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{scopeErr: errors.New("db down")}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: d})
	ctx := context.Background()

	if _, err := orch.ReadKeywordPerformance(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a scope lookup failure must fail the read, not proceed unscoped")
	}
	if _, err := orch.ReadAudienceInsights(ctx, "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); err == nil {
		t.Error("a scope lookup failure must fail the audience read too")
	}
	if d.keywordCalls != 0 || d.audienceCalls != 0 {
		t.Errorf("platform was contacted (%d keyword, %d audience) after the scope could not be "+
			"established", d.keywordCalls, d.audienceCalls)
	}
}
