// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// msLeverDispatcher stands in for a NON-atomic keyword-lever adapter: it reports the outcomes
// it is given, positionally, and records whether it was reached.
type msLeverDispatcher struct {
	err          error
	outcomes     []string
	negOutcomes  []model.NegativeKeywordOutcome
	gotActions   []model.KeywordAction
	gotNegatives []model.NegativeKeyword
}

func (d *msLeverDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *msLeverDispatcher) ApplyKeywordActions(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, actions []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	d.gotActions = actions
	if d.err != nil {
		return nil, d.err
	}
	out := make([]model.KeywordActionOutcome, 0, len(actions))
	for i, a := range actions {
		o := model.KeywordActionOutcome{AdGroupID: a.AdGroupID, CriterionID: a.CriterionID, Action: a.Action, Outcome: d.outcomes[i]}
		if o.Outcome == model.KeywordOutcomeFailed {
			o.ErrorCode = "CampaignServiceInvalidKeywordId"
		}
		out = append(out, o)
	}
	return out, nil
}

func (d *msLeverDispatcher) AddNegativeKeywords(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, keywords []model.NegativeKeyword) ([]model.NegativeKeywordOutcome, error) {
	d.gotNegatives = keywords
	if d.err != nil {
		return nil, d.err
	}
	return d.negOutcomes, nil
}

func TestApplyKeywordActions_MicrosoftRendersPerActionOutcomes(t *testing.T) {
	d := &msLeverDispatcher{outcomes: []string{model.KeywordOutcomeApplied, model.KeywordOutcomeFailed, model.KeywordOutcomeUnconfirmed}}
	s := keywordActionService(t, model.ProviderMicrosoftAds, d)
	res, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "654", CriterionID: "701", Action: "PAUSE"},
		&briefs.KeywordActionInput{AdGroupID: "654", CriterionID: "702", Action: "REMOVE"},
		&briefs.KeywordActionInput{AdGroupID: "654", CriterionID: "703", Action: "PAUSE"},
	))
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("results = %d, want one per action", len(res.Results))
	}
	// applied_count counts only APPLIED on a non-atomic platform.
	if res.AppliedCount != 1 {
		t.Errorf("AppliedCount = %d, want 1", res.AppliedCount)
	}
	want := []string{"APPLIED", "FAILED", "UNCONFIRMED"}
	for i, r := range res.Results {
		if r.Outcome == nil || *r.Outcome != want[i] {
			t.Errorf("results[%d].Outcome = %v, want %s", i, r.Outcome, want[i])
		}
		if r.ResourceName != nil {
			t.Errorf("results[%d] carries a resource name Microsoft never returned: %q", i, *r.ResourceName)
		}
	}
	if res.Results[1].ErrorCode == nil || *res.Results[1].ErrorCode != "CampaignServiceInvalidKeywordId" {
		t.Errorf("the FAILED result must carry its error code, got %v", res.Results[1].ErrorCode)
	}
}

// Google's response must be exactly what it was before the Microsoft fields existed: a resource
// name on every result, no outcome, no error code, and applied_count equal to the request.
func TestApplyKeywordActions_GoogleResultsCarryNoOutcomeFields(t *testing.T) {
	s := keywordActionService(t, model.ProviderGoogleAds, &keywordActionDispatcher{})
	res, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "333", CriterionID: "777", Action: "PAUSE"},
	))
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	r := res.Results[0]
	if r.ResourceName == nil || *r.ResourceName != "customers/1/adGroupCriteria/333~777" {
		t.Errorf("ResourceName = %v", r.ResourceName)
	}
	if r.Outcome != nil || r.ErrorCode != nil || res.AppliedCount != 1 {
		t.Errorf("Google result gained Microsoft fields: %+v (applied %d)", r, res.AppliedCount)
	}
	// The WIRE body, through the generated encoder: byte-identical to the pre-LFXV2-2665 shape.
	body, _ := json.Marshal(briefsserver.NewApplyKeywordActionsResponseBody(res))
	want := `{"campaign_id":"c1","results":[{"ad_group_id":"333","criterion_id":"777","action":"PAUSE","resource_name":"customers/1/adGroupCriteria/333~777"}],"applied_count":1}`
	if string(body) != want {
		t.Errorf("Google wire body = %s\nwant              %s", body, want)
	}
}

func TestApplyKeywordActions_MicrosoftUnconfirmedTellsCallerToVerify(t *testing.T) {
	d := &msLeverDispatcher{err: &unconfirmedKeywordActionError{err: errors.New("504")}}
	s := keywordActionService(t, model.ProviderMicrosoftAds, d)
	_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
		&briefs.KeywordActionInput{AdGroupID: "654", CriterionID: "701", Action: "REMOVE"},
	))
	se, ok := err.(*briefs.ConnServiceUnavailableError)
	if !ok || !strings.Contains(se.Message, "unconfirmed") {
		t.Fatalf("want the unconfirmed 503, got %T: %v", err, err)
	}
}

func TestApplyKeywordActions_MicrosoftConnectionMessagesNameMicrosoft(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{domain.ErrNotFound, "microsoft advertising"},
		{domain.ErrConnectionNotUsable, "microsoft advertising"},
	} {
		s := keywordActionService(t, model.ProviderMicrosoftAds, &msLeverDispatcher{err: tc.err})
		_, err := s.ApplyKeywordActions(context.Background(), keywordActionPayload(
			&briefs.KeywordActionInput{AdGroupID: "654", CriterionID: "701", Action: "PAUSE"},
		))
		if msg := leverErrMessage(err); !strings.Contains(msg, tc.want) || strings.Contains(msg, "google") {
			t.Errorf("%v: message = %q, want it to name %s and not google", tc.err, msg, tc.want)
		}
	}
}

// ---- add-negative-keywords ---------------------------------------------------

func negativePayload(keywords ...*briefs.NegativeKeywordInput) *briefs.AddNegativeKeywordsPayload {
	return &briefs.AddNegativeKeywordsPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", NegativeKeywords: keywords}
}

func TestAddNegativeKeywords_HappyPathIsPositional(t *testing.T) {
	d := &msLeverDispatcher{negOutcomes: []model.NegativeKeywordOutcome{
		{Text: "free", MatchType: "Exact", Outcome: model.KeywordOutcomeApplied, NegativeKeywordID: "9001"},
		{Text: "cheap", MatchType: "Phrase", Outcome: model.KeywordOutcomeAlreadyPresent},
		{Text: "kubernetes", MatchType: "Exact", Outcome: model.KeywordOutcomeFailed, ErrorCode: "CampaignServiceNegativeKeywordMatchesKeyword"},
	}}
	s := keywordActionService(t, model.ProviderMicrosoftAds, d)
	res, err := s.AddNegativeKeywords(context.Background(), negativePayload(
		&briefs.NegativeKeywordInput{Text: "free", MatchType: "Exact"},
		&briefs.NegativeKeywordInput{Text: "cheap", MatchType: "Phrase"},
		&briefs.NegativeKeywordInput{Text: "kubernetes", MatchType: "Exact"},
	))
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if res.CampaignID != "c1" || len(res.Results) != 3 || res.AppliedCount != 2 {
		t.Fatalf("result = %+v (applied %d), want 3 results and applied_count 2", res, res.AppliedCount)
	}
	if res.Results[0].NegativeKeywordID == nil || *res.Results[0].NegativeKeywordID != "9001" || res.Results[1].Outcome != "ALREADY_PRESENT" ||
		res.Results[2].ErrorCode == nil || *res.Results[2].ErrorCode != "CampaignServiceNegativeKeywordMatchesKeyword" {
		t.Errorf("results = %+v", res.Results)
	}
	if len(d.gotNegatives) != 3 || d.gotNegatives[1].MatchType != "Phrase" {
		t.Errorf("dispatcher received %+v", d.gotNegatives)
	}
}

// Every platform but Microsoft is a clean 400, and no dispatcher is reached — including
// Google's, whose adapter has no negative-keyword add for a live campaign.
func TestAddNegativeKeywords_UnsupportedPlatformIsBadRequest(t *testing.T) {
	for _, platform := range []model.Provider{model.ProviderGoogleAds, model.ProviderRedditAds} {
		d := &msLeverDispatcher{}
		s := keywordActionService(t, platform, d)
		_, err := s.AddNegativeKeywords(context.Background(), negativePayload(&briefs.NegativeKeywordInput{Text: "free", MatchType: "Exact"}))
		if _, ok := err.(*briefs.BadRequestError); !ok {
			t.Fatalf("%s: err = %T (%v), want *briefs.BadRequestError", platform, err, err)
		}
		if d.gotNegatives != nil {
			t.Errorf("%s: the dispatcher was reached", platform)
		}
	}
}

// A Microsoft-registered dispatcher WITHOUT the capability answers the same clean 400 through
// the orchestrator's assertion.
func TestAddNegativeKeywords_DispatcherWithoutCapabilityIsBadRequest(t *testing.T) {
	s := keywordActionService(t, model.ProviderMicrosoftAds, &keywordActionDispatcher{})
	_, err := s.AddNegativeKeywords(context.Background(), negativePayload(&briefs.NegativeKeywordInput{Text: "free", MatchType: "Exact"}))
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("err = %T (%v), want *briefs.BadRequestError", err, err)
	}
}

func TestAddNegativeKeywords_EmptyBatchIsBadRequest(t *testing.T) {
	d := &msLeverDispatcher{}
	s := keywordActionService(t, model.ProviderMicrosoftAds, d)
	if _, err := s.AddNegativeKeywords(context.Background(), negativePayload()); err == nil {
		t.Fatal("an empty batch must be refused")
	}
	if d.gotNegatives != nil {
		t.Error("the dispatcher was reached for an empty batch")
	}
}

func TestAddNegativeKeywords_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want any
		msg  string
	}{
		{"invalid", domain.ErrNegativeKeywordInvalid, &briefs.BadRequestError{}, "not valid"},
		{"unprovisioned", domain.ErrCampaignNotProvisioned, &briefs.ConflictError{}, "provisioned"},
		{"provenance", errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch), &briefs.ConflictError{}, "re-dispatched"},
		{"account mismatch", domain.ErrCampaignAccountMismatch, &briefs.ConflictError{}, "reconnect"},
		{"no connection", domain.ErrNotFound, &briefs.NotFoundError{}, "microsoft advertising"},
		{"unconfirmed", &unconfirmedKeywordActionError{err: errors.New("429")}, &briefs.ConnServiceUnavailableError{}, "unconfirmed"},
		{"definite", errors.New("400 from microsoft"), &briefs.ConnServiceUnavailableError{}, "could not be added"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := keywordActionService(t, model.ProviderMicrosoftAds, &msLeverDispatcher{err: tc.err})
			_, err := s.AddNegativeKeywords(context.Background(), negativePayload(&briefs.NegativeKeywordInput{Text: "free", MatchType: "Exact"}))
			if err == nil {
				t.Fatal("expected an error")
			}
			if fmt.Sprintf("%T", err) != fmt.Sprintf("%T", tc.want) {
				t.Fatalf("err = %T (%v), want %T", err, err, tc.want)
			}
			if msg := leverErrMessage(err); !strings.Contains(msg, tc.msg) || strings.Contains(msg, "400 from microsoft") {
				t.Errorf("message = %q, want it to contain %q and no adapter text", msg, tc.msg)
			}
		})
	}
}

// A decrypt failure on credentials that came from the LF system row is logged against that row,
// with the requester beside it, as the bid and budget handlers log it (PR #265 review).
func TestAddNegativeKeywords_SystemRowDecryptFailureIsAttributedToTheSystemRow(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := fmt.Errorf("%w: %w", domain.ErrSystemConnectionOrigin, domain.ErrCredentialDecryptionFailed)
	s := keywordActionService(t, model.ProviderMicrosoftAds, &msLeverDispatcher{err: err})
	if _, aerr := s.AddNegativeKeywords(context.Background(), negativePayload(&briefs.NegativeKeywordInput{Text: "free", MatchType: "Exact"})); aerr == nil {
		t.Fatal("expected an error")
	}
	logged := buf.String()
	if !strings.Contains(logged, "project_id="+model.SystemProjectID) {
		t.Errorf("the failing row is the LF system row, but the log blames the requester.\nlog: %s", logged)
	}
	if !strings.Contains(logged, "requested_by_project_id=cncf") {
		t.Errorf("the requester must stay visible alongside the failing row.\nlog: %s", logged)
	}

	// A project-owned row is still attributed to the project.
	if got := credentialOwnerProject(domain.ErrCredentialDecryptionFailed, "cncf"); got != "cncf" {
		t.Errorf("credentialOwnerProject(own row) = %q, want cncf", got)
	}
}

// A short outcome slice cannot be zipped back onto the request and is UNCONFIRMED.
func TestAddNegativeKeywords_ShortOutcomeSliceIsUnconfirmed(t *testing.T) {
	s := keywordActionService(t, model.ProviderMicrosoftAds, &msLeverDispatcher{negOutcomes: nil})
	_, err := s.AddNegativeKeywords(context.Background(), negativePayload(&briefs.NegativeKeywordInput{Text: "free", MatchType: "Exact"}))
	se, ok := err.(*briefs.ConnServiceUnavailableError)
	if !ok || !strings.Contains(se.Message, "unconfirmed") {
		t.Fatalf("want the unconfirmed 503, got %T: %v", err, err)
	}
}

// leverErrMessage returns the caller-facing Message of a generated briefs error (their Error()
// is empty by Goa's design).
func leverErrMessage(err error) string {
	switch e := err.(type) {
	case *briefs.BadRequestError:
		return e.Message
	case *briefs.ConflictError:
		return e.Message
	case *briefs.NotFoundError:
		return e.Message
	case *briefs.ConnServiceUnavailableError:
		return e.Message
	case *briefs.InternalServerError:
		return e.Message
	}
	return ""
}
