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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// targetingWriterDispatcher implements ONLY TargetingWriter; snapshot, when set, is recorded on
// the row the way the LinkedIn adapter records what it sent.
type targetingWriterDispatcher struct {
	err       error
	calls     int
	gotChange model.TargetingChange
	snapshot  json.RawMessage
}

func (d *targetingWriterDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *targetingWriterDispatcher) WriteTargeting(_ context.Context, _ string, _ model.Provider, c *model.Campaign, change model.TargetingChange) error {
	d.calls++
	d.gotChange = change
	if d.err == nil && d.snapshot != nil {
		c.ConfigSnapshot = d.snapshot
	}
	return d.err
}

func targetingCampaign() *model.Campaign {
	c := budgetCampaign()
	c.Platform = model.ProviderLinkedInAds
	return c
}

func targetingPayload(include []map[string][]string) *briefs.UpdateCampaignTargetingPayload {
	im := "3"
	return &briefs.UpdateCampaignTargetingPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
		Targeting:  &briefs.LinkedinTargetingUpdate{Include: include},
		GeoTargets: []string{"urn:li:geo:103644278"},
	}
}

var skillsClause = []map[string][]string{{"skills": {"urn:li:skill:17"}}}

func TestUpdateCampaignTargeting_HappyPathForwardsAndPersistsTheSnapshot(t *testing.T) {
	d := &targetingWriterDispatcher{snapshot: json.RawMessage(`{"targeting":{"include":[{"skills":["urn:li:skill:17"]}]}}`)}
	s, camps := budgetService(t, targetingCampaign(), d)
	ctx := ctxWithActor(&model.Actor{Username: "mrautela"})
	res, err := s.UpdateCampaignTargeting(ctx, targetingPayload(skillsClause))
	if err != nil {
		t.Fatalf("UpdateCampaignTargeting: %v", err)
	}
	if d.calls != 1 || len(d.gotChange.Include) != 1 || d.gotChange.GeoTargets[0] != "urn:li:geo:103644278" {
		t.Fatalf("dispatcher got %+v after %d call(s)", d.gotChange, d.calls)
	}
	if camps.got == nil || string(camps.got.ConfigSnapshot) != string(d.snapshot) {
		t.Fatalf("persisted snapshot = %v, want the adapter's", camps.got)
	}
	if camps.got.UpdatedBy == nil || camps.got.UpdatedBy.Username != "mrautela" || res.Version != 4 {
		t.Errorf("UpdatedBy = %+v, version = %d", camps.got.UpdatedBy, res.Version)
	}
}

// The segment facets name ad-account resources: a caller without global_marketing_ops is refused
// 403, an unanswerable check 503, both before the row is loaded or LinkedIn reached. Professional
// facets need no check at all.
func TestUpdateCampaignTargeting_SegmentFacetsRequireMarketingOps(t *testing.T) {
	segments := []map[string][]string{{"skills": {"urn:li:skill:17"}}, {"dynamicSegments": {"urn:li:adSegment:9"}}}
	for _, tc := range []struct {
		name    string
		checker *fakeMarketingOps
		include []map[string][]string
		exclude map[string][]string
		want    func(error) bool
		calls   int
	}{
		{"denied include", &fakeMarketingOps{}, segments, nil, func(e error) bool {
			var f *briefs.ForbiddenError
			return errors.As(e, &f) && strings.Contains(f.Message, "targeting.include[1].dynamicSegments")
		}, 0},
		{"denied exclude", &fakeMarketingOps{}, skillsClause, map[string][]string{"audienceMatchingSegments": {"urn:li:adSegment:9"}}, func(e error) bool { var f *briefs.ForbiddenError; return errors.As(e, &f) }, 0},
		{"check failed", &fakeMarketingOps{err: errors.New("nats down")}, segments, nil, func(e error) bool { var u *briefs.ConnServiceUnavailableError; return errors.As(e, &u) }, 0},
		{"granted", &fakeMarketingOps{allow: true}, segments, nil, func(e error) bool { return e == nil }, 1},
		{"no segments, no check", &fakeMarketingOps{}, skillsClause, nil, func(e error) bool { return e == nil }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &targetingWriterDispatcher{}
			s, _ := budgetService(t, targetingCampaign(), d)
			s.SetMarketingOpsChecker(tc.checker)
			p := targetingPayload(tc.include)
			p.Targeting.Exclude = tc.exclude
			_, err := s.UpdateCampaignTargeting(ctxWithActor(&model.Actor{Username: "ed"}), p)
			if !tc.want(err) {
				t.Fatalf("err = %T %v", err, err)
			}
			if d.calls != tc.calls {
				t.Errorf("dispatcher called %d times, want %d", d.calls, tc.calls)
			}
		})
	}
}

func TestUpdateCampaignTargeting_DispatcherErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want func(error) bool
	}{
		{"unsupported", ErrTargetingUnsupported, isBadRequest},
		{"rejected", ErrTargetingRejected, isBadRequest},
		{"unwritable", ErrTargetingUnwritable, isConflict},
		{"unknown provenance", domain.ErrCampaignProvenanceUnknown, isConflict},
		{"account mismatch", ErrCampaignAccountMismatch, isConflict},
		{"absent upstream", domain.ErrPlatformCampaignAbsent, isNotFound},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, isInternal},
		{"definite platform failure", errors.New("linkedin returned 403"), isUnavailable},
		// A bid sentinel is not this endpoint's and must not be dressed up as a targeting refusal.
		{"bid sentinel", ErrBidUnwritable, isUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &targetingWriterDispatcher{err: tc.err}
			s, camps := budgetService(t, targetingCampaign(), d)
			_, err := s.UpdateCampaignTargeting(context.Background(), targetingPayload(skillsClause))
			if err == nil || !tc.want(err) {
				t.Fatalf("unexpected error %T: %v", err, err)
			}
			if camps.got != nil {
				t.Error("the row must not be written when the platform change did not happen")
			}
			if camps.releases != 1 || len(camps.cooldowns) != 0 {
				t.Errorf("releases = %d, cooldowns = %v; want one release", camps.releases, camps.cooldowns)
			}
		})
	}
}

// The adapter's reason reaches the caller; nothing else from the chain does.
func TestUpdateCampaignTargeting_RejectionCarriesTheAdapterReasonOnly(t *testing.T) {
	d := &targetingWriterDispatcher{err: reasonedRejection{reason: "targeting: include[0] is empty"}}
	s, _ := budgetService(t, targetingCampaign(), d)
	_, err := s.UpdateCampaignTargeting(context.Background(), targetingPayload(skillsClause))
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "include[0] is empty") || strings.Contains(bad.Message, "upstream-secret") {
		t.Fatalf("err = %v", err)
	}
}

type reasonedRejection struct{ reason string }

func (r reasonedRejection) Error() string                   { return "upstream-secret " + r.reason }
func (r reasonedRejection) Unwrap() error                   { return ErrTargetingRejected }
func (r reasonedRejection) TargetingRejectedReason() string { return r.reason }

func TestUpdateCampaignTargeting_UnconfirmedHoldsTheLockAndDoesNotPersist(t *testing.T) {
	d := &targetingWriterDispatcher{err: unconfirmedErr{}}
	s, camps := budgetService(t, targetingCampaign(), d)
	_, err := s.UpdateCampaignTargeting(context.Background(), targetingPayload(skillsClause))
	var u *briefs.ConnServiceUnavailableError
	if !errors.As(err, &u) || !strings.Contains(u.Message, "verify the targeting") {
		t.Fatalf("err = %v", err)
	}
	if camps.got != nil || camps.releases != 0 || len(camps.cooldowns) != 1 {
		t.Errorf("got=%v releases=%d cooldowns=%v", camps.got, camps.releases, camps.cooldowns)
	}
}

func TestUpdateCampaignTargeting_UnsupportedPlatformIsBadRequest(t *testing.T) {
	camp := targetingCampaign()
	camp.Platform = model.ProviderMicrosoftAds
	s, camps := budgetService(t, camp, plainDispatcher{})
	_, err := s.UpdateCampaignTargeting(context.Background(), targetingPayload(skillsClause))
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "LinkedIn campaigns only") || camps.got != nil {
		t.Fatalf("err = %v, written = %v", err, camps.got != nil)
	}
}

func TestUpdateCampaignTargeting_RefusedStatesBeforeTheClaim(t *testing.T) {
	stale := targetingPayload(skillsClause)
	s4 := "4"
	stale.IfMatch = &s4
	unprovisioned := targetingCampaign()
	unprovisioned.PlatformCampaignID = ""
	pending := targetingCampaign()
	pending.Status = model.CampaignStatusPending
	for _, tc := range []struct {
		name string
		camp *model.Campaign
		p    *briefs.UpdateCampaignTargetingPayload
	}{
		{"stale etag", targetingCampaign(), stale},
		{"unprovisioned", unprovisioned, targetingPayload(skillsClause)},
		{"pending", pending, targetingPayload(skillsClause)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &targetingWriterDispatcher{}
			s, _ := budgetService(t, tc.camp, d)
			if _, err := s.UpdateCampaignTargeting(context.Background(), tc.p); err == nil {
				t.Fatal("accepted")
			}
			if d.calls != 0 {
				t.Error("the platform was contacted for a refusal the row decides")
			}
		})
	}
}
