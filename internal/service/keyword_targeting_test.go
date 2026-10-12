// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// kwtDispatcher stands in for a keyword-TARGETING adapter (Reddit / X).
type kwtDispatcher struct {
	err         error
	targeting   *model.KeywordTargeting
	outcomes    []model.KeywordTargetingOutcome
	gotRemovals []model.KeywordTargetingRemoval
	gotRevision string
	reached     bool
}

func (d *kwtDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *kwtDispatcher) ReadKeywordTargeting(context.Context, string, model.Provider, *model.Campaign) (*model.KeywordTargeting, error) {
	d.reached = true
	if d.err != nil {
		return nil, d.err
	}
	return d.targeting, nil
}

func (d *kwtDispatcher) RemoveKeywordTargeting(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, removals []model.KeywordTargetingRemoval, revision string) ([]model.KeywordTargetingOutcome, error) {
	d.reached = true
	d.gotRemovals, d.gotRevision = removals, revision
	if d.err != nil {
		return nil, d.err
	}
	return d.outcomes, nil
}

func strp(s string) *string { return &s }

func kwtRemovePayload(revision *string, items ...*briefs.KeywordTargetingRemovalInput) *briefs.RemoveKeywordTargetingPayload {
	return &briefs.RemoveKeywordTargetingPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Keywords: items, Revision: revision}
}

func TestGetKeywordTargeting_RendersRedditAndX(t *testing.T) {
	d := &kwtDispatcher{targeting: &model.KeywordTargeting{EntityID: "t5_ag", Revision: "sha256:abc",
		Keywords: []model.KeywordTargetingEntry{{Keyword: "kubernetes"}}}}
	s := keywordActionService(t, model.ProviderRedditAds, d)
	res, err := s.GetKeywordTargeting(context.Background(), &briefs.GetKeywordTargetingPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1"})
	if err != nil {
		t.Fatalf("GetKeywordTargeting: %v", err)
	}
	body, _ := json.Marshal(briefsserver.NewGetKeywordTargetingResponseBody(res))
	want := `{"campaign_id":"c1","platform":"reddit-ads","targeting_entity_id":"t5_ag","keywords":[{"keyword":"kubernetes"}],"revision":"sha256:abc"}`
	if string(body) != want {
		t.Errorf("Reddit wire body = %s\nwant              %s", body, want)
	}

	d = &kwtDispatcher{targeting: &model.KeywordTargeting{EntityID: "li1",
		Keywords: []model.KeywordTargetingEntry{{Keyword: "ebpf", CriterionID: "k1", MatchType: "EXACT_KEYWORD"}}}}
	s = keywordActionService(t, model.ProviderTwitterAds, d)
	res, err = s.GetKeywordTargeting(context.Background(), &briefs.GetKeywordTargetingPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1"})
	if err != nil {
		t.Fatalf("GetKeywordTargeting: %v", err)
	}
	body, _ = json.Marshal(briefsserver.NewGetKeywordTargetingResponseBody(res))
	want = `{"campaign_id":"c1","platform":"twitter-ads","targeting_entity_id":"li1","keywords":[{"keyword":"ebpf","criterion_id":"k1","match_type":"EXACT_KEYWORD"}]}`
	if string(body) != want {
		t.Errorf("X wire body = %s\nwant          %s", body, want)
	}
}

// Google Ads and Microsoft Advertising keywords are criteria, reached through
// apply-keyword-actions; neither dispatcher is ever asked about targeting.
func TestKeywordTargeting_OtherPlatformsAre400WithoutContactingTheDispatcher(t *testing.T) {
	for _, p := range []model.Provider{model.ProviderGoogleAds, model.ProviderMicrosoftAds, model.ProviderMetaAds, model.ProviderLinkedInAds} {
		d := &kwtDispatcher{}
		s := keywordActionService(t, p, d)
		_, gerr := s.GetKeywordTargeting(context.Background(), &briefs.GetKeywordTargetingPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1"})
		_, rerr := s.RemoveKeywordTargeting(context.Background(), kwtRemovePayload(nil, &briefs.KeywordTargetingRemovalInput{Keyword: strp("a")}))
		for _, err := range []error{gerr, rerr} {
			if _, ok := err.(*briefs.BadRequestError); !ok {
				t.Errorf("%s: want 400, got %T: %v", p, err, err)
			}
		}
		if d.reached {
			t.Errorf("%s: the dispatcher was reached", p)
		}
	}
}

func TestRemoveKeywordTargeting_RendersPerItemOutcomesPositionally(t *testing.T) {
	d := &kwtDispatcher{outcomes: []model.KeywordTargetingOutcome{
		{CriterionID: "k1", Outcome: model.KeywordOutcomeApplied},
		{CriterionID: "k2", Outcome: model.KeywordOutcomeFailed, ErrorCode: model.KeywordTargetingErrNotFound},
		{CriterionID: "k3", Outcome: model.KeywordOutcomeUnconfirmed},
		{CriterionID: "k4", Outcome: "SOMETHING_ELSE"},
	}}
	s := keywordActionService(t, model.ProviderTwitterAds, d)
	res, err := s.RemoveKeywordTargeting(context.Background(), kwtRemovePayload(nil,
		&briefs.KeywordTargetingRemovalInput{CriterionID: strp("k1")},
		&briefs.KeywordTargetingRemovalInput{CriterionID: strp("k2")},
		&briefs.KeywordTargetingRemovalInput{CriterionID: strp("k3")},
		&briefs.KeywordTargetingRemovalInput{CriterionID: strp("k4")},
	))
	if err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if res.AppliedCount != 1 || len(res.Results) != 4 {
		t.Fatalf("applied %d of %d results", res.AppliedCount, len(res.Results))
	}
	// An outcome the adapter did not name is never reported as success.
	for i, want := range []string{"APPLIED", "FAILED", "UNCONFIRMED", "UNCONFIRMED"} {
		if res.Results[i].Outcome != want {
			t.Errorf("results[%d].Outcome = %s, want %s", i, res.Results[i].Outcome, want)
		}
	}
	if res.Results[1].ErrorCode == nil || *res.Results[1].ErrorCode != "NOT_FOUND" || res.Results[0].Keyword != nil {
		t.Errorf("result fields: %+v / %+v", res.Results[0], res.Results[1])
	}
	if len(d.gotRemovals) != 4 || d.gotRemovals[2].CriterionID != "k3" || d.gotRevision != "" {
		t.Errorf("the dispatcher got %+v rev %q", d.gotRemovals, d.gotRevision)
	}
}

func TestRemoveKeywordTargeting_PassesTheRedditRevision(t *testing.T) {
	d := &kwtDispatcher{outcomes: []model.KeywordTargetingOutcome{{Keyword: "a", Outcome: model.KeywordOutcomeApplied}}}
	s := keywordActionService(t, model.ProviderRedditAds, d)
	if _, err := s.RemoveKeywordTargeting(context.Background(), kwtRemovePayload(strp("sha256:abc"),
		&briefs.KeywordTargetingRemovalInput{Keyword: strp("a")})); err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if d.gotRevision != "sha256:abc" || d.gotRemovals[0].Keyword != "a" {
		t.Errorf("dispatcher got %+v rev %q", d.gotRemovals, d.gotRevision)
	}
}

// A short outcome slice is detected AFTER the removal was issued, so it is UNCONFIRMED.
func TestRemoveKeywordTargeting_ShortOutcomesAreUnconfirmed(t *testing.T) {
	d := &kwtDispatcher{outcomes: []model.KeywordTargetingOutcome{{CriterionID: "k1", Outcome: model.KeywordOutcomeApplied}}}
	s := keywordActionService(t, model.ProviderTwitterAds, d)
	_, err := s.RemoveKeywordTargeting(context.Background(), kwtRemovePayload(nil,
		&briefs.KeywordTargetingRemovalInput{CriterionID: strp("k1")},
		&briefs.KeywordTargetingRemovalInput{CriterionID: strp("k2")},
	))
	se, ok := err.(*briefs.ConnServiceUnavailableError)
	if !ok || !strings.Contains(se.Message, "unconfirmed") {
		t.Fatalf("want the unconfirmed 503, got %T: %v", err, err)
	}
}

type kwtUnconfirmed struct{}

func (kwtUnconfirmed) Error() string     { return "upstream said: secret-token-xyz" }
func (kwtUnconfirmed) Unconfirmed() bool { return true }

func TestKeywordTargeting_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err      error
		wantType string
		want     string
	}{
		{domain.ErrKeywordTargetingUnsupported, "*lfxv2campaignservicebriefs.BadRequestError", "not supported or not enabled"},
		{fmt.Errorf("x: %w", domain.ErrKeywordTargetingInvalid), "*lfxv2campaignservicebriefs.BadRequestError", "not valid"},
		{domain.ErrKeywordTargetingChanged, "*lfxv2campaignservicebriefs.ConflictError", "changed since it was read"},
		{domain.ErrKeywordTargetingWouldEmpty, "*lfxv2campaignservicebriefs.ConflictError", "every keyword"},
		{domain.ErrKeywordTargetingUnaddressable, "*lfxv2campaignservicebriefs.ConflictError", "cannot be addressed"},
		{domain.ErrCampaignNotProvisioned, "*lfxv2campaignservicebriefs.ConflictError", "not fully provisioned"},
		{errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch), "*lfxv2campaignservicebriefs.ConflictError", "re-dispatched"},
		{domain.ErrCampaignAccountMismatch, "*lfxv2campaignservicebriefs.ConflictError", "different ad account"},
		{domain.ErrNotFound, "*lfxv2campaignservicebriefs.NotFoundError", "x ads connection"},
		{domain.ErrConnectionNotUsable, "*lfxv2campaignservicebriefs.ConflictError", "x ads connection"},
		{kwtUnconfirmed{}, "*lfxv2campaignservicebriefs.ConnServiceUnavailableError", "read the keyword targeting again"},
		{errors.New("upstream said: secret-token-xyz"), "*lfxv2campaignservicebriefs.ConnServiceUnavailableError", "could not be removed"},
	} {
		d := &kwtDispatcher{err: tc.err}
		s := keywordActionService(t, model.ProviderTwitterAds, d)
		_, err := s.RemoveKeywordTargeting(context.Background(), kwtRemovePayload(nil, &briefs.KeywordTargetingRemovalInput{CriterionID: strp("k1")}))
		if got := fmt.Sprintf("%T", err); got != tc.wantType {
			t.Errorf("%v: type %s, want %s", tc.err, got, tc.wantType)
		}
		msg := leverErrMessage(err)
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%v: message %q, want it to contain %q", tc.err, msg, tc.want)
		}
		// Upstream text is never echoed to the caller.
		if strings.Contains(msg, "secret-token-xyz") {
			t.Errorf("%v: message echoes upstream text: %q", tc.err, msg)
		}
	}
}
