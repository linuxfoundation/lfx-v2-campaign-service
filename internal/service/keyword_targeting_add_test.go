// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// kwtAddDispatcher stands in for the X positive keyword add adapter (#564 gap 12).
type kwtAddDispatcher struct {
	kwtDispatcher
	gotKeywords []string
}

func (d *kwtAddDispatcher) AddKeywordTargeting(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, keywords []string) ([]model.KeywordTargetingOutcome, error) {
	d.reached = true
	d.gotKeywords = keywords
	if d.err != nil {
		return nil, d.err
	}
	return d.outcomes, nil
}

func kwtAddPayload(keywords ...string) *briefs.AddKeywordTargetingPayload {
	return &briefs.AddKeywordTargetingPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Keywords: keywords}
}

// The add is X-only: every other platform, Reddit included, is 400 before the dispatcher.
func TestAddKeywordTargeting_NonXIs400WithoutContactingTheDispatcher(t *testing.T) {
	for _, p := range []model.Provider{model.ProviderRedditAds, model.ProviderGoogleAds, model.ProviderMicrosoftAds, model.ProviderMetaAds, model.ProviderLinkedInAds} {
		d := &kwtAddDispatcher{}
		s := keywordActionService(t, p, d)
		_, err := s.AddKeywordTargeting(context.Background(), kwtAddPayload("kubernetes"))
		if _, ok := err.(*briefs.BadRequestError); !ok {
			t.Errorf("%s: want 400, got %T: %v", p, err, err)
		}
		if d.reached {
			t.Errorf("%s: the dispatcher was reached", p)
		}
	}
}

// The create-time keyword rules apply before anything is contacted, and the adapter receives the
// normalized list: trimmed, case-folded duplicates dropped.
func TestAddKeywordTargeting_NormalizesAndRefusesBeforeTheDispatcher(t *testing.T) {
	for name, kws := range map[string][]string{
		"blank":    {"  "},
		"too long": {strings.Repeat("k", 81)},
		"control":  {"a\tb"},
	} {
		d := &kwtAddDispatcher{}
		s := keywordActionService(t, model.ProviderTwitterAds, d)
		_, err := s.AddKeywordTargeting(context.Background(), kwtAddPayload(kws...))
		if br, ok := err.(*briefs.BadRequestError); !ok || !strings.Contains(br.Message, "keywords:") {
			t.Errorf("%s: want a 400 naming the keywords field, got %T: %v", name, err, err)
		}
		if d.reached {
			t.Errorf("%s: the dispatcher was reached", name)
		}
	}

	d := &kwtAddDispatcher{kwtDispatcher: kwtDispatcher{outcomes: []model.KeywordTargetingOutcome{
		{Keyword: "Kubernetes", CriterionID: "k1", Outcome: model.KeywordOutcomeAlreadyPresent},
		{Keyword: "ebpf", CriterionID: "k9", Outcome: model.KeywordOutcomeApplied},
	}}}
	s := keywordActionService(t, model.ProviderTwitterAds, d)
	res, err := s.AddKeywordTargeting(context.Background(), kwtAddPayload(" Kubernetes ", "kubernetes", "ebpf"))
	if err != nil {
		t.Fatalf("AddKeywordTargeting: %v", err)
	}
	if fmt.Sprint(d.gotKeywords) != "[Kubernetes ebpf]" {
		t.Errorf("adapter got %q, want the normalized list", d.gotKeywords)
	}
	body, _ := json.Marshal(briefsserver.NewAddKeywordTargetingResponseBody(res))
	want := `{"campaign_id":"c1","results":[{"keyword":"Kubernetes","criterion_id":"k1","outcome":"ALREADY_PRESENT"},{"keyword":"ebpf","criterion_id":"k9","outcome":"APPLIED"}],"applied_count":2}`
	if string(body) != want {
		t.Errorf("wire body = %s\nwant       %s", body, want)
	}
}

// Per-item outcomes render positionally; FAILED and UNCONFIRMED carry no criterion id and do not
// count, and an outcome the adapter did not name is UNCONFIRMED.
func TestAddKeywordTargeting_RendersOutcomes(t *testing.T) {
	d := &kwtAddDispatcher{kwtDispatcher: kwtDispatcher{outcomes: []model.KeywordTargetingOutcome{
		{Keyword: "a", CriterionID: "x1", Outcome: model.KeywordOutcomeApplied},
		{Keyword: "b", CriterionID: "x2", Outcome: model.KeywordOutcomeFailed, ErrorCode: model.KeywordTargetingErrRejected},
		{Keyword: "c", CriterionID: "x3", Outcome: "SOMETHING_ELSE"},
	}}}
	s := keywordActionService(t, model.ProviderTwitterAds, d)
	res, err := s.AddKeywordTargeting(context.Background(), kwtAddPayload("a", "b", "c"))
	if err != nil {
		t.Fatalf("AddKeywordTargeting: %v", err)
	}
	body, _ := json.Marshal(briefsserver.NewAddKeywordTargetingResponseBody(res))
	want := `{"campaign_id":"c1","results":[{"keyword":"a","criterion_id":"x1","outcome":"APPLIED"},{"keyword":"b","outcome":"FAILED","error_code":"REJECTED"},{"keyword":"c","outcome":"UNCONFIRMED"}],"applied_count":1}`
	if string(body) != want {
		t.Errorf("wire body = %s\nwant       %s", body, want)
	}
}

// The adapter's refusals map to fixed messages: the gate off and an invalid batch are 400, the
// criteria cap and a contended lock 409, an all-UNCONFIRMED add 503 worded as unconfirmed.
func TestAddKeywordTargeting_ClassifiesAdapterErrors(t *testing.T) {
	for _, tc := range []struct {
		err      error
		want     string
		contains string
	}{
		{fmt.Errorf("x: %w", domain.ErrKeywordTargetingUnsupported), "*lfxv2campaignservicebriefs.BadRequestError", "not enabled"},
		{fmt.Errorf("x: %w", domain.ErrKeywordTargetingInvalid), "*lfxv2campaignservicebriefs.BadRequestError", "additions are not valid"},
		{fmt.Errorf("x: %w", domain.ErrKeywordTargetingLimit), "*lfxv2campaignservicebriefs.ConflictError", "60 targeting criteria"},
		{fmt.Errorf("x: %w", domain.ErrKeywordTargetingWriteInProgress), "*lfxv2campaignservicebriefs.ConflictError", "nothing was added"},
		{&testUnconfirmed{}, "*lfxv2campaignservicebriefs.ConnServiceUnavailableError", "additions are unconfirmed"},
	} {
		d := &kwtAddDispatcher{kwtDispatcher: kwtDispatcher{err: tc.err}}
		s := keywordActionService(t, model.ProviderTwitterAds, d)
		_, err := s.AddKeywordTargeting(context.Background(), kwtAddPayload("ebpf"))
		if got := fmt.Sprintf("%T", err); got != tc.want || !strings.Contains(addErrMessage(err), tc.contains) {
			t.Errorf("%v: got %s %q, want %s containing %q", tc.err, got, addErrMessage(err), tc.want, tc.contains)
		}
	}
}

type testUnconfirmed struct{}

func (*testUnconfirmed) Error() string     { return "unconfirmed" }
func (*testUnconfirmed) Unconfirmed() bool { return true }

// addErrMessage is the caller-facing message of a briefs service error.
func addErrMessage(err error) string {
	switch e := err.(type) {
	case *briefs.BadRequestError:
		return e.Message
	case *briefs.ConflictError:
		return e.Message
	case *briefs.ConnServiceUnavailableError:
		return e.Message
	}
	return ""
}
