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
	"time"

	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// adSetDispatcher stands in for the Meta adapter's two ad-set capabilities.
type adSetDispatcher struct {
	readErr   error
	read      *model.MetaAdSets
	toggleErr error
	toggle    *model.MetaAdSetStatusResult
	reached   int
	gotStatus string
	gotAdSet  string
	gotWindow model.MetricsWindow
}

func (d *adSetDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *adSetDispatcher) ReadMetaAdSets(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, w model.MetricsWindow) (*model.MetaAdSets, error) {
	d.reached++
	d.gotWindow = w
	return d.read, d.readErr
}

func (d *adSetDispatcher) ToggleMetaAdSetStatus(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, adSetID, status string) (*model.MetaAdSetStatusResult, error) {
	d.reached++
	d.gotAdSet, d.gotStatus = adSetID, status
	return d.toggle, d.toggleErr
}

// noAdSetDispatcher has neither capability: every non-Meta platform today.
type noAdSetDispatcher struct{ reached bool }

func (d *noAdSetDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	d.reached = true
	return nil, errors.New("unused")
}

// adSetRepo is the toggle fake plus a record of how the claim was released.
type adSetRepo struct {
	*toggleCampaignRepo
	cooldowns int
}

func (r *adSetRepo) ReleaseCampaignLockAfterCooldown(tok domain.CampaignLockToken, d time.Duration) {
	r.cooldowns++
	r.toggleCampaignRepo.ReleaseCampaignLockAfterCooldown(tok, d)
}

// adSetUnconfirmedErr is what the adapter returns for a write whose outcome is unknown.
type adSetUnconfirmedErr struct{ msg string }

func (e adSetUnconfirmedErr) Error() string     { return e.msg }
func (e adSetUnconfirmedErr) Unconfirmed() bool { return true }

func metaCampaignRow() *model.Campaign {
	return &model.Campaign{ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderMetaAds,
		PlatformCampaignID: "555", Status: model.CampaignStatusCreated, Version: 3}
}

func adSetService(camp *model.Campaign, platform model.Provider, d PlatformDispatcher) (*BriefService, *adSetRepo) {
	repo := &adSetRepo{toggleCampaignRepo: &toggleCampaignRepo{got: camp}}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(repo, jobs, map[model.Provider]PlatformDispatcher{platform: d})
	return NewBriefService(newFakeBriefRepo(), repo, jobs, orch), repo
}

func listPayload() *briefs.ListMetaAdSetsPayload {
	return &briefs.ListMetaAdSetsPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1"}
}

func togglePayload(ifMatch, adSet, status string) *briefs.ToggleMetaAdSetStatusPayload {
	p := &briefs.ToggleMetaAdSetStatusPayload{ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", AdSetID: adSet, Status: status}
	if ifMatch != "" {
		p.IfMatch = &ifMatch
	}
	return p
}

// ---- list ------------------------------------------------------------------

func TestListMetaAdSets_RendersTheWireBody(t *testing.T) {
	daily := model.BudgetDaily
	name := "Leads US"
	d := &adSetDispatcher{read: &model.MetaAdSets{
		PlatformCampaignID: "555", Window: model.MetricsWindowLast7Days, Currency: "USD",
		ReadAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC),
		AdSets: []model.MetaAdSet{
			{ID: "888", Listed: true, Name: &name, BudgetType: &daily, BudgetAmount: strp("50.00"), Impressions: 1000, Clicks: 20, CostMicros: 7250000, Ctr: 0.02, Recorded: true},
			{ID: "890", Impressions: 10, Clicks: 1, CostMicros: 500000, Ctr: 0.1},
		},
	}}
	s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	w := "last_7_days"
	p := listPayload()
	p.Window = &w
	res, err := s.ListMetaAdSets(context.Background(), p)
	if err != nil {
		t.Fatalf("ListMetaAdSets: %v", err)
	}
	body, _ := json.Marshal(briefsserver.NewListMetaAdSetsResponseBody(res))
	want := `{"campaign_id":"c1","platform_campaign_id":"555","window":"last_7_days","currency":"USD","read_at":"2026-10-08T09:30:00Z","ad_sets":[` +
		`{"id":"888","listed":true,"name":"Leads US","budget_type":"daily","budget_amount":"50.00","impressions":1000,"clicks":20,"cost_micros":7250000,"ctr":0.02,"recorded":true},` +
		`{"id":"890","listed":false,"impressions":10,"clicks":1,"cost_micros":500000,"ctr":0.1,"recorded":false}],"ad_set_count":2}`
	if string(body) != want {
		t.Errorf("wire body = %s\nwant        %s", body, want)
	}
	if d.gotWindow != model.MetricsWindowLast7Days {
		t.Errorf("window = %q", d.gotWindow)
	}
}

func TestListMetaAdSets_DefaultsTheWindowAndValidatesIt(t *testing.T) {
	d := &adSetDispatcher{read: &model.MetaAdSets{}}
	s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	if _, err := s.ListMetaAdSets(context.Background(), listPayload()); err != nil || d.gotWindow != model.MetricsWindowLast30Days {
		t.Fatalf("err = %v, window = %q", err, d.gotWindow)
	}
	bad := "last_90_days"
	p := listPayload()
	p.Window = &bad
	if _, err := s.ListMetaAdSets(context.Background(), p); !isStatus(err, 400) {
		t.Fatalf("err = %T %v, want 400", err, err)
	}
}

func TestListMetaAdSets_StatusMapping(t *testing.T) {
	t.Run("non-meta campaign is 400 without reaching any dispatcher", func(t *testing.T) {
		d := &noAdSetDispatcher{}
		row := metaCampaignRow()
		row.Platform = model.ProviderGoogleAds
		row.PlatformCampaignID = ""
		s, _ := adSetService(row, model.ProviderGoogleAds, d)
		if _, err := s.ListMetaAdSets(context.Background(), listPayload()); !isStatus(err, 400) || d.reached {
			t.Fatalf("err = %T %v, reached = %v", err, err, d.reached)
		}
	})
	t.Run("unprovisioned row is 409 without reaching the adapter", func(t *testing.T) {
		d := &adSetDispatcher{}
		row := metaCampaignRow()
		row.PlatformCampaignID = ""
		s, _ := adSetService(row, model.ProviderMetaAds, d)
		if _, err := s.ListMetaAdSets(context.Background(), listPayload()); !isStatus(err, 409) || d.reached != 0 {
			t.Fatalf("err = %T %v, reached = %d", err, err, d.reached)
		}
	})
	t.Run("a missing campaign row is the only 404", func(t *testing.T) {
		d := &adSetDispatcher{}
		s, repo := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
		repo.getErr = domain.ErrNotFound
		if _, err := s.ListMetaAdSets(context.Background(), listPayload()); !isStatus(err, 404) || d.reached != 0 {
			t.Fatalf("err = %T %v", err, err)
		}
	})
	const secret = "upstream-secret-fbtrace-XYZ"
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown provenance", errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch, errors.New(secret)), 409},
		{"account mismatch", fmt.Errorf("%s: %w", secret, domain.ErrCampaignAccountMismatch), 409},
		{"upstream identity mismatch", fmt.Errorf("%s: %w", secret, domain.ErrCampaignUpstreamIdentityMismatch), 409},
		{"no connection is 409, never 404", fmt.Errorf("%s: %w", secret, domain.ErrNotFound), 409},
		{"connection not usable", fmt.Errorf("%s: %w", secret, domain.ErrConnectionNotUsable), 409},
		{"no account selected", fmt.Errorf("%w: %w: %s", domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, secret), 409},
		{"system connection not usable", fmt.Errorf("%w: %w: %s", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable, secret), 500},
		{"decryption", fmt.Errorf("%s: %w", secret, domain.ErrCredentialDecryptionFailed), 500},
		{"Graph 100/33 or any upstream failure is 503, never 404", errors.New("meta API GET /555/adsets failed (400): " + secret + " (code: 100)"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &adSetDispatcher{readErr: tc.err}
			s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
			_, err := s.ListMetaAdSets(context.Background(), listPayload())
			if !isStatus(err, tc.want) {
				t.Fatalf("err = %T %v, want %d", err, err, tc.want)
			}
			if strings.Contains(errMessage(err), secret) {
				t.Fatalf("client message leaks upstream text: %q", errMessage(err))
			}
		})
	}
}

// ---- toggle ----------------------------------------------------------------

func TestToggleMetaAdSetStatus_AppliedReturnsTheUnchangedETag(t *testing.T) {
	d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, repo := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED"))
	if err != nil {
		t.Fatalf("ToggleMetaAdSetStatus: %v", err)
	}
	if d.gotAdSet != "888" || d.gotStatus != "PAUSED" {
		t.Errorf("adapter got (%q, %q)", d.gotAdSet, d.gotStatus)
	}
	body, _ := json.Marshal(briefsserver.NewToggleMetaAdSetStatusResponseBody(res))
	if want := `{"campaign_id":"c1","ad_set_id":"888","requested_status":"PAUSED","previous_status":"ACTIVE","outcome":"APPLIED"}`; string(body) != want {
		t.Errorf("wire body = %s\nwant        %s", body, want)
	}
	// Nothing is persisted: the ETag is the row's unchanged version, and the row was never written.
	if res.Etag == nil || *res.Etag != `"3"` || repo.replaced != nil || repo.got.Version != 3 {
		t.Errorf("etag = %v, replaced = %+v, version = %d", res.Etag, repo.replaced, repo.got.Version)
	}
	if repo.cooldowns != 0 {
		t.Errorf("an applied write held the lock for a cooldown")
	}
}

func TestToggleMetaAdSetStatus_AlreadyInState(t *testing.T) {
	d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{AdSetID: "888", Outcome: model.MetaAdSetAlreadyInState, PreviousStatus: "PAUSED"}}
	s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload(`"3"`, "888", "PAUSED"))
	if err != nil || res.Outcome != "ALREADY_IN_STATE" || *res.Etag != `"3"` {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestToggleMetaAdSetStatus_UnconfirmedIsReportedAndHoldsTheLock(t *testing.T) {
	d := &adSetDispatcher{toggleErr: fmt.Errorf("wrapped: %w", adSetUnconfirmedErr{msg: "meta API POST /888 failed (429)"})}
	s, repo := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
	if err != nil || res.Outcome != "UNCONFIRMED" || res.PreviousStatus != nil || *res.Etag != `"3"` {
		t.Fatalf("got %+v, %v; want a 200 UNCONFIRMED outcome", res, err)
	}
	if repo.cooldowns != 1 {
		t.Errorf("cooldown releases = %d, want 1", repo.cooldowns)
	}
}

func TestToggleMetaAdSetStatus_UnrecognisedOutcomeIsNeverSuccess(t *testing.T) {
	d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{Outcome: "DONE"}}
	s, _ := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
	res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
	if err != nil || res.Outcome != "UNCONFIRMED" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestToggleMetaAdSetStatus_RefusalsBeforeTheAdapter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*model.Campaign)
		platform model.Provider
		payload  *briefs.ToggleMetaAdSetStatusPayload
		want     int
	}{
		{"missing If-Match is 428", nil, model.ProviderMetaAds, togglePayload("", "888", "PAUSED"), 428},
		{"stale If-Match is 412", nil, model.ProviderMetaAds, togglePayload("2", "888", "PAUSED"), 412},
		{"malformed ad set id is 400", nil, model.ProviderMetaAds, togglePayload("3", "0888", "PAUSED"), 400},
		{"path-shaped ad set id is 400", nil, model.ProviderMetaAds, togglePayload("3", "888/ads", "PAUSED"), 400},
		{"lowercase status is 400", nil, model.ProviderMetaAds, togglePayload("3", "888", "paused"), 400},
		{"non-meta campaign is 400", func(c *model.Campaign) { c.Platform = model.ProviderRedditAds }, model.ProviderRedditAds, togglePayload("3", "888", "PAUSED"), 400},
		{"unprovisioned is 409", func(c *model.Campaign) { c.PlatformCampaignID = "" }, model.ProviderMetaAds, togglePayload("3", "888", "PAUSED"), 409},
		{"pending campaign is 409", func(c *model.Campaign) { c.Status = model.CampaignStatusPending }, model.ProviderMetaAds, togglePayload("3", "888", "PAUSED"), 409},
		{"activate under a degraded campaign is 409", func(c *model.Campaign) { c.Status = model.CampaignStatusCreatedDegraded }, model.ProviderMetaAds, togglePayload("3", "888", "ACTIVE"), 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := metaCampaignRow()
			if tc.mutate != nil {
				tc.mutate(row)
			}
			d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{Outcome: model.MetaAdSetApplied}}
			var disp PlatformDispatcher = d
			if tc.platform != model.ProviderMetaAds {
				disp = &noAdSetDispatcher{}
			}
			s, repo := adSetService(row, tc.platform, disp)
			_, err := s.ToggleMetaAdSetStatus(context.Background(), tc.payload)
			if !isStatus(err, tc.want) {
				t.Fatalf("err = %T %v, want %d", err, err, tc.want)
			}
			if d.reached != 0 || repo.replaced != nil {
				t.Fatalf("adapter reached %d time(s), replaced = %+v", d.reached, repo.replaced)
			}
		})
	}
}

func TestToggleMetaAdSetStatus_PauseUnderADegradedCampaignIsAllowed(t *testing.T) {
	row := metaCampaignRow()
	row.Status = model.CampaignStatusCreatedDegraded
	d := &adSetDispatcher{toggle: &model.MetaAdSetStatusResult{Outcome: model.MetaAdSetApplied, PreviousStatus: "ACTIVE"}}
	s, _ := adSetService(row, model.ProviderMetaAds, d)
	if res, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "PAUSED")); err != nil || res.Outcome != "APPLIED" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// Every client message is a fixed sentence; the adapter's text (which may carry Graph's own
// message and fbtrace id) is logged, never returned.
func TestToggleMetaAdSetStatus_ErrorMappingUsesFixedText(t *testing.T) {
	const secret = "Graph says: invalid token fbtrace_id=SECRET123"
	for _, tc := range []struct {
		name     string
		err      error
		want     int
		contains string
	}{
		{"adopted activate", fmt.Errorf("%w: %s", domain.ErrCampaignNotProvisioned, secret), 409, "ADOPTED"},
		{"not this campaign's ad set", fmt.Errorf("%s: %w", secret, domain.ErrMetaAdSetNotInCampaign), 409, "does not belong to this campaign"},
		{"deleted ad set", fmt.Errorf("%s: %w", secret, domain.ErrMetaAdSetUnwritable), 409, "deleted or archived"},
		{"invalid ad set id", fmt.Errorf("%s: %w", secret, domain.ErrMetaAdSetInvalid), 400, "ad_set_id"},
		{"unknown provenance", errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch, errors.New(secret)), 409, "does not record which ad account"},
		{"account mismatch", fmt.Errorf("%s: %w", secret, domain.ErrCampaignAccountMismatch), 409, "different ad account"},
		{"definite refusal or not sent", errors.New(secret), 503, "nothing was changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &adSetDispatcher{toggleErr: tc.err}
			s, repo := adSetService(metaCampaignRow(), model.ProviderMetaAds, d)
			_, err := s.ToggleMetaAdSetStatus(context.Background(), togglePayload("3", "888", "ACTIVE"))
			if msg := errMessage(err); !isStatus(err, tc.want) || !strings.Contains(msg, tc.contains) || strings.Contains(msg, "SECRET123") {
				t.Fatalf("err = %T %q, want %d containing %q and no upstream text", err, msg, tc.want, tc.contains)
			}
			if repo.cooldowns != 0 || repo.replaced != nil {
				t.Fatalf("a definite failure held the lock (%d) or wrote the row (%+v)", repo.cooldowns, repo.replaced)
			}
		})
	}
}

// isStatus reports whether err is the generated error type for code.
func isStatus(err error, code int) bool {
	switch e := err.(type) {
	case *briefs.BadRequestError:
		return code == 400 && e.Code == "400"
	case *briefs.NotFoundError:
		return code == 404
	case *briefs.ConflictError:
		return code == 409
	case *briefs.PreconditionFailedError:
		return code == 412
	case *briefs.PreconditionRequiredError:
		return code == 428
	case *briefs.InternalServerError:
		return code == 500
	case *briefs.ConnServiceUnavailableError:
		return code == 503
	}
	return false
}

// errMessage is the client-facing message of a generated error (their Error() is empty).
func errMessage(err error) string {
	switch e := err.(type) {
	case *briefs.BadRequestError:
		return e.Message
	case *briefs.NotFoundError:
		return e.Message
	case *briefs.ConflictError:
		return e.Message
	case *briefs.PreconditionFailedError:
		return e.Message
	case *briefs.PreconditionRequiredError:
		return e.Message
	case *briefs.InternalServerError:
		return e.Message
	case *briefs.ConnServiceUnavailableError:
		return e.Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
