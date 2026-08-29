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

func (d *keywordActionDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.KeywordPerformance, error) {
	if d.err != nil {
		return nil, d.err
	}
	// Every field carries a DISTINCT non-zero value. A zero-valued fixture cannot tell a
	// mapper that forwards a field from one that drops it, because both produce zero — the
	// whole point of this stub is to make a dropped field visible in the response.
	score := int64(7)
	return &model.KeywordPerformance{Window: model.MetricsWindowLast30Days, Rows: []model.KeywordRow{{
		CriterionID:  "777",
		AdGroupID:    "333",
		AdGroupName:  "Registration - Exact",
		CampaignName: "KubeCon NA 2026 - Search",
		QualityScore: &score,
		Conversions:  12.5,
	}}}, nil
}

func (d *keywordActionDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.AudienceInsights, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.AudienceInsights{Window: model.MetricsWindowLast30Days, Buckets: []model.AudienceBucket{{
		Dimension:   model.AudienceDimensionAge,
		Value:       "AGE_RANGE_25_34",
		Conversions: 31.25,
	}}}, nil
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
	se, ok := err.(*briefs.ConnServiceUnavailableError)
	if !ok {
		t.Fatalf("error = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
	}
	// ...and it must be the UNCONFIRMED 503, not the definite-failure one. By the time the
	// count can be short the mutate has already been ISSUED, so the atomic batch may be fully
	// applied with only the accounting lost. Both arms are 503 by design, so asserting the type
	// alone would pass against a plain error — the MESSAGE is what separates "verify first"
	// from "retry", and retrying an irreversible REMOVE that already ran is the wrong remedy.
	if se.Message == "the keyword actions could not be applied" {
		t.Fatalf("a possibly-applied mutation answered the DEFINITE-failure message %q, inviting a "+
			"retry of a batch that may already have run", se.Message)
	}
	if !strings.Contains(se.Message, "unconfirmed") || !strings.Contains(se.Message, "verify") {
		t.Errorf("message must report the outcome as unconfirmed and tell the caller to verify, got %q", se.Message)
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

// TestInsightsAccountMismatch_RemedyCoversMixedProvenance pins the REMEDY, not just the
// status code, because the code was already right while the instruction was wrong.
//
// googleAdsScopeForCustomer raises this sentinel on ANY mismatch, so the common case is a
// MIXED one: some of the project's campaigns sit under the account the connection now
// resolves to, others under an older one. "Reconnect the original account" is actively
// harmful advice there — it would only swap which subset mismatches, breaking the campaigns
// that currently work. The remedy has to name reconciling the mismatched campaign rows, and
// may offer reconnecting only for the case where one account owns all of them.
//
// The ad account ids must still stay out of the message: which account a project connects to
// is connection configuration, not something a keyword read discloses.
func TestInsightsAccountMismatch_RemedyCoversMixedProvenance(t *testing.T) {
	svc := keywordInsightsService(t, &keywordActionDispatcher{
		err: errors.Join(ErrCampaignAccountMismatch, errors.New("customer 7777777777 vs 1234567890")),
	})

	_, err := svc.GetGoogleAdsKeywords(context.Background(), &conn.GetGoogleAdsKeywordsPayload{ProjectID: "cncf"})
	ce, ok := err.(*conn.ConflictError)
	if !ok {
		t.Fatalf("error = %T (%v), want *conn.ConflictError", err, err)
	}
	msg := strings.ToLower(ce.Message)
	// The mismatch is partial-capable, so the message must not assert that every campaign is
	// in the other account.
	if strings.Contains(msg, "this project's campaigns belong to a different") {
		t.Errorf("message asserts ALL campaigns mismatch, but the sentinel is raised on a "+
			"PARTIAL mismatch too: %q", ce.Message)
	}
	// It must point at reconciling/re-dispatching the offending rows — the only remedy that
	// works when some campaigns already match the current account.
	if !strings.Contains(msg, "reconcile") && !strings.Contains(msg, "re-dispatch") {
		t.Errorf("message does not tell the caller to reconcile or re-dispatch the mismatched "+
			"campaigns; reconnecting alone breaks the campaigns that currently match: %q", ce.Message)
	}
	if strings.Contains(ce.Message, "7777777777") || strings.Contains(ce.Message, "1234567890") {
		t.Errorf("response disclosed an ad account id: %q", ce.Message)
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

// unconfirmedKeywordActionError mirrors the dispatch layer's unexported unconfirmedToggleError:
// an ambiguous outcome is signalled across the package boundary by BEHAVIOUR (an Unconfirmed()
// method found with errors.As), not by a shared sentinel. A test in this package cannot
// construct the dispatch type, so it reproduces the contract the service actually matches on.
type unconfirmedKeywordActionError struct{ err error }

func (e *unconfirmedKeywordActionError) Error() string {
	return "keyword action outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *unconfirmedKeywordActionError) Unwrap() error     { return e.err }
func (e *unconfirmedKeywordActionError) Unconfirmed() bool { return true }

// An UNCONFIRMED mutate must NOT answer the same thing a definite failure does.
//
// The client reports UNCONFIRMED when a keyword mutate MAY already have been applied (a short
// or mismatched mutate response, a 5xx, a timeout). A plain 503 reads as "retry", and retrying
// a REMOVE that already ran is the wrong remedy — Google cannot re-enable a removed criterion,
// only create a new one with a new id. So the ambiguous answer must be distinguishable from the
// definite one and must tell the caller to VERIFY first.
//
// Asserted on the MESSAGE, because both arms are 503 by design (the endpoint's declared error
// set is unchanged): a test that checked only the status code would pass against the defect.
func TestApplyKeywordActions_UnconfirmedTellsCallerToVerifyNotRetry(t *testing.T) {
	upstream := errors.New("adGroupCriteria:mutate returned 2 results for a 3-action batch")
	s := keywordActionService(t, model.ProviderGoogleAds,
		&keywordActionDispatcher{err: &unconfirmedKeywordActionError{err: upstream}})

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "REMOVE"},
	))
	if err == nil {
		t.Fatal("expected an error for an unconfirmed mutate, got nil")
	}
	se, ok := err.(*briefs.ConnServiceUnavailableError)
	if !ok {
		t.Fatalf("error = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
	}
	// The definite-failure arm's message. Answering it here is exactly the defect: it tells a
	// caller whose REMOVE may already have run to retry it.
	if se.Message == "the keyword actions could not be applied" {
		t.Fatalf("an UNCONFIRMED outcome answered the DEFINITE-failure message %q — a caller whose "+
			"irreversible REMOVE may already have applied is being told to retry", se.Message)
	}
	if !strings.Contains(se.Message, "unconfirmed") {
		t.Errorf("message must say the outcome is unconfirmed, got %q", se.Message)
	}
	if !strings.Contains(se.Message, "verify") {
		t.Errorf("message must tell the caller to verify before retrying, got %q", se.Message)
	}
	// The adapter's own text names upstream detail and must never be echoed.
	if strings.Contains(se.Message, "adGroupCriteria") {
		t.Errorf("response echoed adapter detail: %q", se.Message)
	}
}

// A PERMANENT input fault must dominate a CONTINGENT state fault, and the two layers must
// agree about it.
//
// The dispatcher validates the batch before it checks provisioning or resolves a connection,
// deliberately — "so a permanent input fault masks any contingent connection fault rather than
// the other way round" (dispatch/googleads.go). The orchestrator used to run its own
// provisioning guard FIRST, inverting that: a malformed batch against an unprovisioned campaign
// answered 409 ("try later") instead of 400 ("fix your request"), so the caller retried forever
// on input only they could correct.
//
// The dispatcher here validates before it reports provisioning, reproducing the real adapter's
// documented ordering — so if the orchestrator preempts it, this test sees the 409.
func TestApplyKeywordActions_MalformedBatchBeatsUnprovisionedCampaign(t *testing.T) {
	d := &orderedKeywordActionDispatcher{
		invalid:         domain.ErrKeywordActionInvalid,
		notProvisioned:  domain.ErrCampaignNotProvisioned,
		invalidCriteria: "999",
	}
	s := keywordActionServiceUnprovisioned(t, model.ProviderGoogleAds, d)

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "999", Action: "PAUSE"},
	))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if ce, ok := err.(*briefs.ConflictError); ok {
		t.Fatalf("a malformed batch against an unprovisioned campaign answered 409 %q — the "+
			"contingent state fault masked the permanent input fault, so the caller is told to "+
			"retry input they must fix", ce.Message)
	}
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("error = %T (%v), want *briefs.BadRequestError", err, err)
	}
	if !d.validated {
		t.Error("the dispatcher never got to validate the batch — the orchestrator refused it first")
	}
}

// A VALID batch against an unprovisioned campaign must STILL answer 409. The reordering above
// must not cost the provisioning guard: only the malformed case changes.
func TestApplyKeywordActions_ValidBatchStillReportsUnprovisioned(t *testing.T) {
	d := &orderedKeywordActionDispatcher{
		invalid:         domain.ErrKeywordActionInvalid,
		notProvisioned:  domain.ErrCampaignNotProvisioned,
		invalidCriteria: "999",
	}
	s := keywordActionServiceUnprovisioned(t, model.ProviderGoogleAds, d)

	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
	))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if _, ok := err.(*briefs.ConflictError); !ok {
		t.Fatalf("error = %T (%v), want *briefs.ConflictError — a valid batch against an "+
			"unprovisioned campaign is still a state conflict", err, err)
	}
}

// A nil campaign is refused by the orchestrator itself rather than delegated: it is not an
// input fault a caller can fix, and no adapter should have to nil-check before validating.
func TestApplyKeywordActions_NilCampaignIsRefusedBeforeTheDispatcher(t *testing.T) {
	d := &orderedKeywordActionDispatcher{
		invalid:        domain.ErrKeywordActionInvalid,
		notProvisioned: domain.ErrCampaignNotProvisioned,
	}
	orch := NewOrchestrator(&fakeCampaignRepo{byID: map[string]*model.Campaign{}}, newFakeJobRepo(),
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: d})

	_, err := orch.ApplyKeywordActions(context.Background(), "cncf", model.ProviderGoogleAds, nil,
		[]model.KeywordAction{{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"}})
	if !errors.Is(err, ErrCampaignNotProvisioned) {
		t.Fatalf("err = %v, want ErrCampaignNotProvisioned", err)
	}
	if d.called {
		t.Error("the dispatcher was handed a nil campaign")
	}
}

// orderedKeywordActionDispatcher reproduces the real Google Ads adapter's DOCUMENTED ordering:
// it validates the batch BEFORE it reports the campaign's provisioning state. That order is the
// contract under test — a dispatcher that checked provisioning first would make the orchestrator
// look correct no matter what it does.
type orderedKeywordActionDispatcher struct {
	invalid         error
	notProvisioned  error
	invalidCriteria string

	validated bool
	called    bool
}

func (d *orderedKeywordActionDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *orderedKeywordActionDispatcher) ApplyKeywordActions(_ context.Context, _ string, _ model.Provider, campaign *model.Campaign, actions []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	d.called = true
	// Validate FIRST — the permanent fault dominates.
	d.validated = true
	for _, a := range actions {
		if d.invalidCriteria != "" && a.CriterionID == d.invalidCriteria {
			return nil, d.invalid
		}
	}
	// Provisioning SECOND — the contingent fault.
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, d.notProvisioned
	}
	out := make([]model.KeywordActionOutcome, 0, len(actions))
	for _, a := range actions {
		out = append(out, model.KeywordActionOutcome{
			AdGroupID: a.AdGroupID, CriterionID: a.CriterionID, Action: a.Action,
			ResourceName: "customers/1/adGroupCriteria/" + a.AdGroupID + "~" + a.CriterionID,
		})
	}
	return out, nil
}

// keywordActionServiceUnprovisioned wires a service whose campaign row carries NO upstream
// platform campaign id — the state that used to short-circuit the orchestrator's guard.
func keywordActionServiceUnprovisioned(t *testing.T, platform model.Provider, d PlatformDispatcher) *BriefService {
	t.Helper()
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{
		"c1": {ID: "c1", Platform: platform, PlatformCampaignID: ""},
	}}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{platform: d})
	return NewBriefService(repo, camps, jobs, orch)
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
	// The fields added for the UI cutover must survive the model→wire mapping. This is a
	// SEPARATE layer from the client decode: the platform client can populate all four and
	// this mapper still drop them, and because the generated struct's fields are values the
	// compiler reports nothing — the response simply carries zeros.
	got := res.Rows[0]
	if got.AdGroupName != "Registration - Exact" {
		t.Errorf("AdGroupName = %q, want %q", got.AdGroupName, "Registration - Exact")
	}
	if got.CampaignName != "KubeCon NA 2026 - Search" {
		t.Errorf("CampaignName = %q, want %q", got.CampaignName, "KubeCon NA 2026 - Search")
	}
	if got.QualityScore == nil {
		t.Errorf("QualityScore = nil, want 7")
	} else if *got.QualityScore != 7 {
		t.Errorf("QualityScore = %d, want 7", *got.QualityScore)
	}
	if got.Conversions != 12.5 {
		t.Errorf("Conversions = %v, want 12.5", got.Conversions)
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
	if got := res.Buckets[0].Conversions; got != 31.25 {
		t.Errorf("Conversions = %v, want 31.25", got)
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
		// Reachable once the scope filter refuses ANY campaign — googleAdsScopeForCustomer
		// fails closed on a partial mismatch too, rather than returning the matching subset as
		// though it were the project's whole picture. PERMANENT until the rows and the
		// connection are reconciled, so it must be a 409 rather than the 503 default, which
		// would invite retrying a read that will keep failing.
		{"a campaign in another account", ErrCampaignAccountMismatch, &conn.ConflictError{}},
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
	case *conn.ConflictError:
		if _, ok := err.(*conn.ConflictError); !ok {
			t.Fatalf("error = %T (%v), want *conn.ConflictError", err, err)
		}
	default:
		// Without this arm an unlisted `want` type asserts NOTHING and its row passes no
		// matter what the classifier returns — a test that agrees with any implementation.
		// That is not hypothetical: the ConflictError row above was added first and silently
		// passed with its classification arm deleted, because the switch had no case for it.
		t.Fatalf("assertInsightsErr has no case for want type %T; the row asserted nothing", want)
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

func (d nilResultDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.KeywordPerformance, error) {
	return nil, nil
}

func (d nilResultDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.AudienceInsights, error) {
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

func (d nilSliceDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.KeywordPerformance, error) {
	return &model.KeywordPerformance{Window: model.MetricsWindowLast30Days}, nil
}

func (d nilSliceDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.AudienceInsights, error) {
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

// An unprovisioned campaign is still refused with ErrCampaignNotProvisioned — but WHICH LAYER
// refuses it differs, and that is the point of this test.
//
// This previously asserted that the ORCHESTRATOR refused both rows ahead of the dispatcher, and
// it passed only because its fake (keywordActionDispatcher) ignores the campaign argument
// entirely and never raises the sentinel itself. That pre-dispatch ordering was the defect: it
// let a contingent state fault mask a permanent input fault, so a malformed batch against an
// unprovisioned campaign answered 409 instead of 400 (see
// TestApplyKeywordActions_MalformedBatchBeatsUnprovisionedCampaign).
//
// The contract now: a NIL campaign is refused by the orchestrator itself — no adapter should
// have to nil-check before it can validate — while an empty PlatformCampaignID is DELEGATED to
// the adapter, which validates the batch first and then raises the same sentinel. Both rows use
// a dispatcher that actually implements that ordering, so neither passes by accident.
func TestOrchestratorApplyKeywordActions_UnprovisionedIsRefused(t *testing.T) {
	actions := []model.KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: model.KeywordActionPause}}

	for _, tc := range []struct {
		name     string
		campaign *model.Campaign
		// wantDispatched records whether the refusal is expected to come from the adapter
		// (true) or from the orchestrator ahead of it (false). Asserting this is what keeps
		// the two layers' division of labour pinned rather than merely the sentinel.
		wantDispatched bool
	}{
		{"nil campaign", nil, false},
		{"empty platform campaign id", &model.Campaign{Platform: model.ProviderGoogleAds}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &orderedKeywordActionDispatcher{
				invalid:        domain.ErrKeywordActionInvalid,
				notProvisioned: domain.ErrCampaignNotProvisioned,
			}
			orch := keywordOrchestrator(d)
			_, err := orch.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, tc.campaign, actions)
			if !errors.Is(err, ErrCampaignNotProvisioned) {
				t.Fatalf("error = %v, want ErrCampaignNotProvisioned", err)
			}
			if d.called != tc.wantDispatched {
				t.Errorf("dispatcher called = %v, want %v", d.called, tc.wantDispatched)
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
	gotScope      []model.ProjectCampaignScope
}

func (d *countingInsightsDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("not used")
}

func (d *countingInsightsDispatcher) ReadKeywordPerformance(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.KeywordPerformance, error) {
	d.keywordCalls++
	d.gotScope = scope
	return &model.KeywordPerformance{Window: w, Rows: []model.KeywordRow{{CriterionID: "leaked-from-another-project"}}}, nil
}

func (d *countingInsightsDispatcher) ReadAudienceInsights(_ context.Context, _ string, _ model.Provider, w model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.AudienceInsights, error) {
	d.audienceCalls++
	d.gotScope = scope
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
	if len(d.gotScope) != 2 || d.gotScope[0].PlatformCampaignID != "111" || d.gotScope[1].PlatformCampaignID != "222" {
		t.Errorf("adapter received scope %v, want the project's own campaign ids; a dropped "+
			"scope restores the account-wide read", d.gotScope)
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

// ─── platform campaign resolution ───

// resolverService wires a ConnectionService whose campaign repo holds the given campaigns.
//
// The campaigns are supplied as real rows rather than through scopeIDs, because the resolver
// answers from the ROWS: it needs each campaign's own id and brief, which a scope entry does not
// carry.
func resolverService(t *testing.T, campaigns ...*model.Campaign) *ConnectionService {
	t.Helper()
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	camps := &fakeCampaignRepo{upserted: campaigns}
	svc.SetOrchestrator(NewOrchestrator(camps, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: &keywordActionDispatcher{},
	}))
	return svc
}

func googleCampaign(id, briefID, projectID, platformCampaignID string) *model.Campaign {
	return &model.Campaign{
		ID:                 id,
		BriefID:            briefID,
		ProjectID:          projectID,
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: platformCampaignID,
	}
}

func TestResolveGoogleAdsCampaign_ReturnsTheBriefAndCampaignPair(t *testing.T) {
	svc := resolverService(t, googleCampaign("c-1", "b-1", "cncf", "24183781329"))

	res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "24183781329",
	})
	if err != nil {
		t.Fatalf("ResolveGoogleAdsCampaign: %v", err)
	}
	if res.MatchCount != 1 || len(res.Matches) != 1 {
		t.Fatalf("matches = %d, match_count = %d, want 1", len(res.Matches), res.MatchCount)
	}
	// BOTH ids are asserted, and with different values: the pair is the whole point of the
	// endpoint, and a resolver that returned the campaign id twice would satisfy a single-field
	// check while leaving the mutation route unaddressable.
	if res.Matches[0].CampaignID != "c-1" {
		t.Errorf("CampaignID = %q, want c-1", res.Matches[0].CampaignID)
	}
	if res.Matches[0].BriefID != "b-1" {
		t.Errorf("BriefID = %q, want b-1", res.Matches[0].BriefID)
	}
	// Echoed from the request so the caller can pair the answer with what it asked.
	if res.PlatformCampaignID != "24183781329" {
		t.Errorf("PlatformCampaignID = %q, want the requested id", res.PlatformCampaignID)
	}
}

// An id this project does not own answers 200 with NO matches, not an error.
//
// The distinction is what the caller acts on: an empty result means "not your campaign, refuse
// the action", while an error means "the request or the service is wrong, retry or report". A
// 404 here would say the latter about the former.
func TestResolveGoogleAdsCampaign_UnownedIDIsAnEmptyAnswerNotAnError(t *testing.T) {
	svc := resolverService(t, googleCampaign("c-1", "b-1", "cncf", "24183781329"))

	res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "99999999999",
	})
	if err != nil {
		t.Fatalf("an unowned id must not be an error: %v", err)
	}
	if res.MatchCount != 0 {
		t.Errorf("match_count = %d, want 0", res.MatchCount)
	}
	// Non-nil so it serialises as `[]` rather than `null` — the empty case is the one a caller
	// must be able to read without a null check.
	if res.Matches == nil {
		t.Error("Matches = nil, want an empty slice so the JSON carries [] rather than null")
	}
}

// THE TENANT BOUNDARY. The Google Ads customer is shared across foundations, so a campaign id
// belonging to another project must resolve to nothing — otherwise this endpoint answers
// "does foundation X own campaign N?", which is exactly the question the shared account makes
// dangerous.
func TestResolveGoogleAdsCampaign_DoesNotResolveAnotherProjectsCampaign(t *testing.T) {
	svc := resolverService(t, googleCampaign("c-other", "b-other", "another-foundation", "24183781329"))

	res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "24183781329",
	})
	if err != nil {
		t.Fatalf("ResolveGoogleAdsCampaign: %v", err)
	}
	if res.MatchCount != 0 || len(res.Matches) != 0 {
		t.Fatalf("another project's campaign resolved: %+v", res.Matches)
	}
}

// A soft-deleted campaign is invisible here as it is to every other read. Resolving one would
// hand back a handle to a campaign this service considers gone, and the mutation route would
// then refuse it — an error the caller cannot act on, in place of a clean "not found".
func TestResolveGoogleAdsCampaign_SkipsSoftDeletedCampaigns(t *testing.T) {
	deleted := googleCampaign("c-1", "b-1", "cncf", "24183781329")
	deleted.Status = "deleted"
	svc := resolverService(t, deleted)

	res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "24183781329",
	})
	if err != nil {
		t.Fatalf("ResolveGoogleAdsCampaign: %v", err)
	}
	if res.MatchCount != 0 {
		t.Errorf("a soft-deleted campaign was resolved: %+v", res.Matches)
	}
}

// Ambiguity is REPORTED, not resolved.
//
// A valid database cannot produce this: uq_campaigns_platform_campaign_live (migration 000020)
// is a global UNIQUE index on (platform, platform_campaign_id) for live Google Ads rows, so the
// state this fake constructs is one PostgreSQL would reject. The test is therefore about the
// SHAPE of the answer if that invariant ever lapses — a dropped index, a narrowed predicate, a
// platform added to the read without being added to the index.
//
// It is worth pinning precisely because it is unreachable today: the alternative is a handler
// that quietly takes matches[0], which would mutate a campaign the caller never named the first
// time the invariant did lapse. Weaker evidence than a test driving a reachable path, and said
// here rather than left to look like a schema claim.
func TestResolveGoogleAdsCampaign_ReportsAmbiguityRatherThanPickingOne(t *testing.T) {
	svc := resolverService(t,
		googleCampaign("c-1", "b-1", "cncf", "24183781329"),
		googleCampaign("c-2", "b-2", "cncf", "24183781329"),
	)

	res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "24183781329",
	})
	if err != nil {
		t.Fatalf("ResolveGoogleAdsCampaign: %v", err)
	}
	if res.MatchCount != 2 || len(res.Matches) != 2 {
		t.Fatalf("matches = %d, want both rows so the caller can refuse", len(res.Matches))
	}
	seen := map[string]string{}
	for _, m := range res.Matches {
		seen[m.CampaignID] = m.BriefID
	}
	if seen["c-1"] != "b-1" || seen["c-2"] != "b-2" {
		t.Errorf("each match must carry its OWN brief: %+v", seen)
	}
}

// The reserved LF scope is unaddressable here for the same reason it is on the keyword and
// audience reads: left open, it would report whether the Linux Foundation's own scope holds a
// given campaign to any caller.
func TestResolveGoogleAdsCampaign_RejectsSystemScope(t *testing.T) {
	svc := resolverService(t, googleCampaign("c-1", "b-1", model.SystemProjectID, "24183781329"))

	_, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          model.SystemProjectID,
		PlatformCampaignID: "24183781329",
	})
	if err == nil {
		t.Fatal("expected the reserved system scope to be rejected")
	}
	if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("error = %T (%v), want *conn.NotFoundError", err, err)
	}
}

// A STORAGE failure is this service's fault, not the ad platform's.
//
// The resolver contacts no platform — the answer is entirely in this service's tables — so
// routing its error through classifyInsightsError would advertise a local table fault as a
// retryable Google Ads outage, in a message naming "keyword insights". It would also produce a
// 503 that means something else entirely: this method DOES declare one, for a backend that is not
// wired. That is usually cold start and usually clears, but not always — in no-database mode the
// repository is never bound and the same 503 persists for the life of the process, and a JWKS
// outage takes the same disposition. A live database fault is none of those: it is a 500.
func TestResolveGoogleAdsCampaign_StorageFailureIsNotAPlatformOutage(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	camps := &fakeCampaignRepo{resolveErr: errors.New("connection refused")}
	svc.SetOrchestrator(NewOrchestrator(camps, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: &keywordActionDispatcher{},
	}))

	_, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "24183781329",
	})
	if err == nil {
		t.Fatal("a storage failure must not be reported as success")
	}
	// A 500, not the 503 the insights classifier's default arm produces. Asserted on the TYPE
	// rather than the message, because the type is what the generated encoder switches on — a
	// 503 here would be indistinguishable from the cold-start one this method declares, telling
	// the caller to retry a fault that retrying cannot fix.
	if _, ok := err.(*conn.InternalServerError); !ok {
		t.Fatalf("error = %T (%v), want *conn.InternalServerError", err, err)
	}
	// The message must not describe a platform read. "keyword insights" tells the caller to
	// retry against Google, which will never fix a database fault.
	if msg := err.Error(); strings.Contains(strings.ToLower(msg), "keyword insights") {
		t.Errorf("message describes a platform failure: %q", msg)
	}
}

// COLD START is a 503, and the design must declare it or the generated encoder turns it into an
// opaque 500.
//
// `resolveBackendWithOrch` refuses before storage and the orchestrator are wired, which is a
// genuinely retryable condition — distinct from a storage FAULT in a service that is up, which
// is a 500 because retrying does not help. Both reach the caller from this one method, so both
// have to be declared and distinguishable.
func TestResolveGoogleAdsCampaign_ColdStartIsRetryable(t *testing.T) {
	// No SetOrchestrator: the service is constructed but not yet wired, which is the state a
	// request arriving during startup finds.
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})

	_, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
		ProjectID:          "cncf",
		PlatformCampaignID: "24183781329",
	})
	if err == nil {
		t.Fatal("expected a cold-start refusal")
	}
	if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
		t.Fatalf("error = %T (%v), want *conn.ConnServiceUnavailableError — a 500 here tells the caller not to retry something that will succeed once startup finishes", err, err)
	}
}

// A malformed id is a 400, not a confident "no such campaign".
//
// The DSL's `^[0-9]+$` / MaxLength(19) is enforced by the generated HTTP decoder only, so a
// direct service or endpoint caller bypasses it. Unchecked, the value reaches the query and
// returns an empty match set — which this route documents as "this project owns no campaign
// with that id". That is a claim the input never justified, and the caller cannot then tell a
// typo from an unowned campaign.
func TestResolveGoogleAdsCampaign_MalformedIDIsRefusedNotAnsweredEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"letters", "abc"},
		{"mixed", "2418378132x"},
		{"empty", ""},
		{"twenty digits, one past the int64 width", "12345678901234567890"},
		{"negative", "-1"},
		{"decimal", "24183781329.0"},
		{"leading space", " 24183781329"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := resolverService(t, googleCampaign("c-1", "b-1", "cncf", "24183781329"))

			res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
				ProjectID:          "cncf",
				PlatformCampaignID: tc.id,
			})
			if err == nil {
				t.Fatalf("a malformed id answered %+v instead of being refused", res)
			}
			if _, ok := err.(*conn.BadRequestError); !ok {
				t.Errorf("error = %T (%v), want *conn.BadRequestError", err, err)
			}
		})
	}
}

// The other direction, so the guard cannot be satisfied by refusing everything: a real
// 11-digit id and the widest legal one both still resolve.
func TestResolveGoogleAdsCampaign_WellFormedIDsAreStillAccepted(t *testing.T) {
	for _, id := range []string{"24183781329", "1", "9223372036854775807"} {
		svc := resolverService(t, googleCampaign("c-1", "b-1", "cncf", id))

		res, err := svc.ResolveGoogleAdsCampaign(context.Background(), &conn.ResolveGoogleAdsCampaignPayload{
			ProjectID:          "cncf",
			PlatformCampaignID: id,
		})
		if err != nil {
			t.Fatalf("a well-formed id %q was refused: %v", id, err)
		}
		if res.MatchCount != 1 {
			t.Errorf("id %q: match_count = %d, want 1", id, res.MatchCount)
		}
	}
}
