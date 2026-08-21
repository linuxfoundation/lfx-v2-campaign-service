// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// A defect in THIS SERVICE must not reach a caller as a caller-fault status.
//
// domain.ErrTokenRequestRejected documents its own remedy as "this is a service defect, file
// a bug": there is no field on a connection whose editing makes a malformed refresh request
// well-formed. But the sentinel that decides the STATUS is a separate axis, and the dispatch
// boundary used to wrap this reason alongside domain.ErrConnectionNotUsable — whose every
// consumer answers a caller-fault status. So the split created to stop sending operators to
// audit a correct configuration sent them there anyway:
//
//	metrics   409 "...repair the connection before reading metrics"
//	toggle    409 "...repair the connection before changing campaign status"
//	adoption  409 "...repair the connection before adopting a campaign"
//	discovery 400 "the stored ... connection cannot be used as configured"
//
// and the reason token reached only the log. These assert the RESPONSE an operator sees,
// not the error value: the classification was already correct, and the defect was entirely
// in the mapping. A test that checked only errors.Is would have passed throughout.
//
// domain.ErrServiceDefect is what carries the status axis now. It is matched ABOVE the
// general ErrConnectionNotUsable arm at every consumer, so the reason sentinel can keep
// travelling alongside for unusableConnectionReason without the general arm swallowing it.

// serviceDefectErr is the shape internal/dispatch/linkedin.go produces for a token request
// LinkedIn refused on protocol grounds: the status sentinel, the reason sentinel, and the
// originating cause.
func serviceDefectErr() error {
	return fmt.Errorf("%w: %w: %w", domain.ErrServiceDefect, domain.ErrTokenRequestRejected,
		errors.New("linkedin: \"LF LinkedIn\" could not be refreshed because LinkedIn rejected the token REQUEST itself"))
}

func TestServiceDefect_MetricsAnswers500NotAConnectionRepair409(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderLinkedInAds,
		PlatformCampaignID: "li-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	s := newMetricsService(camp, &metricsOnlyDispatcher{err: serviceDefectErr()})
	window := "last_30_days"
	_, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Window: &window,
	})

	var conflict *briefs.ConflictError
	if errors.As(err, &conflict) {
		t.Fatalf("answered 409 %q — this is OUR defect, and that message sends an operator to "+
			"repair a connection with nothing wrong with it", conflict.Message)
	}
	var ise *briefs.InternalServerError
	if !errors.As(err, &ise) {
		t.Fatalf("want 500 (InternalServerError), got %T: %v", err, err)
	}
	if strings.Contains(ise.Message, "repair") || strings.Contains(ise.Message, "reconnect") {
		t.Errorf("the 500 body names a remedy the caller does not own: %q", ise.Message)
	}
}

func TestServiceDefect_ToggleAnswers500NotAConnectionRepair409(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: model.CampaignStatusCreated, Version: 1,
	}
	s, _ := newToggleService(camp, &stubToggler{err: serviceDefectErr()})
	im := "1"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})

	var conflict *briefs.ConflictError
	if errors.As(err, &conflict) {
		t.Fatalf("answered 409 %q — the operator is told to repair a connection before changing "+
			"campaign status, for a request this service built wrongly", conflict.Message)
	}
	var ise *briefs.InternalServerError
	if !errors.As(err, &ise) {
		t.Fatalf("want 500 (InternalServerError), got %T: %v", err, err)
	}
}

// Adoption resolves credentials through the same dispatcher path as metrics and the toggle,
// so it reaches this sentinel the same way — and its 409 is the most misleading of the set,
// because the adoption switch carries THREE other ConflictError arms whose messages each name
// a different remedy the operator does not need ("repair the connection", "select an account",
// "connect your own ad account"). A ConflictError here is indistinguishable from those to the
// caller.
//
// This arm shipped without a test. service_defect_status_test.go covered metrics, the toggle,
// discovery, the brief-metrics row and the async dispatch log, and deleting the adoption arm
// left every one of them green while the misleading 409 came back.
//
// The error is injected through the LOOKUP, which is where the dispatcher resolves credentials:
// LookupPlatformCampaign wraps whatever the adapter returns, so this is the production shape.
func TestServiceDefect_AdoptionAnswers500NotAConnectionRepair409(t *testing.T) {
	disp := &adopterDispatcher{err: serviceDefectErr()}
	s, camps := newAdoptService(t, model.ProviderGoogleAds, disp)

	_, err := s.AdoptCampaign(context.Background(), adoptPayload())

	var conflict *briefs.ConflictError
	if errors.As(err, &conflict) {
		t.Fatalf("answered 409 %q — the operator is sent to repair a connection that is not at "+
			"fault, for a request this service built wrongly", conflict.Message)
	}
	var bad *briefs.BadRequestError
	if errors.As(err, &bad) {
		t.Fatalf("answered 400 %q — a caller-fault status for a defect no caller can repair", bad.Message)
	}
	var ise *briefs.InternalServerError
	if !errors.As(err, &ise) {
		t.Fatalf("want 500 (InternalServerError), got %T: %v", err, err)
	}
	// Nothing may be bound: a defect on the verification read must not persist a binding to a
	// campaign this service never confirmed exists.
	if len(camps.adopted) != 0 {
		t.Errorf("bound %d campaigns despite an unverified lookup, want 0", len(camps.adopted))
	}
}

func TestServiceDefect_DiscoveryAnswers500NotAConfigurationFault400(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderLinkedInAds: &mockAccountListerDispatcher{err: serviceDefectErr()},
		},
	})
	_, err := svc.ListLinkedinAdsAccounts(context.Background(),
		&conn.ListLinkedinAdsAccountsPayload{ProjectID: "cncf"})

	var bad *conn.BadRequestError
	if errors.As(err, &bad) {
		t.Fatalf("answered 400 %q — it tells the operator their stored connection cannot be used "+
			"as configured, and names fields to go and check that are all correct", bad.Message)
	}
	var ise *conn.InternalServerError
	if !errors.As(err, &ise) {
		t.Fatalf("want 500 (InternalServerError), got %T: %v", err, err)
	}
}

// The brief-wide metrics read has no status code per row — it has a STRING an operator
// reads, and "reconnect it" there is the same provably useless remedy a 409 would have been.
func TestServiceDefect_BriefMetricsRowNamesNoConnectionRemedy(t *testing.T) {
	status, reason := classifyBriefMetricsErr(serviceDefectErr(), model.ProviderLinkedInAds)

	if status == "connection_problem" {
		t.Errorf("classified as connection_problem — the connection is not the problem, and that "+
			"status is what a consumer renders as 'go fix your connection' (reason: %q)", reason)
	}
	for _, remedy := range []string{"reconnect", "repair it", "connect it"} {
		if strings.Contains(reason, remedy) {
			t.Errorf("the row's reason names %q, a remedy the reader does not own: %q", remedy, reason)
		}
	}
	if !strings.Contains(reason, "defect in this service") {
		t.Errorf("the reason must say the fault is OURS, or the reader assumes it is theirs: %q", reason)
	}
}

// serviceDefectDispatcher fails the way internal/dispatch/linkedin.go does for a refused
// token request: a pre-create fault carrying the status sentinel and the reason sentinel.
// Unwrap is implemented for the reason accountNotSelectedErr documents — without it
// errors.Is reaches neither sentinel and the test would pass against the very thing it pins.
type serviceDefectDispatcher struct{}

func (serviceDefectDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, accountNotSelectedErr{err: serviceDefectErr()}
}

// The async pre-create path answers nothing — the 202 is long gone — so the LOG is the whole
// diagnosis. Both connection arms assert a connection defect, and emitting one for our own
// malformed request sends whoever reads it to repair a healthy row.
func TestServiceDefect_AsyncDispatchLogDoesNotBlameTheConnection(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderLinkedInAds: serviceDefectDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.recs {
		var reason, gotJob string
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				reason = a.Value.String()
			case "job_id":
				gotJob = a.Value.String()
			}
			return true
		})
		// slog.Default is process-wide; a sibling test's dispatch goroutine can drain here.
		if gotJob != id {
			continue
		}
		if strings.Contains(rec.Message, "the LF system connection is not usable") {
			t.Fatalf("logged the system-connection line for a defect of ours: %q", rec.Message)
		}
		if !strings.Contains(rec.Message, "defect in this service") {
			t.Fatalf("the log must name this service as the faulty party, or whoever reads it "+
				"repairs a healthy connection: %q", rec.Message)
		}
		// The reason token still has to travel: the STATUS axis changed, the vocabulary did not.
		if reason != "token_request_rejected" {
			t.Fatalf("reason = %q, want \"token_request_rejected\" — the machine-readable "+
				"classification is the only diagnosis this path emits", reason)
		}
		return
	}
	t.Fatal("no dispatch-failure log record was emitted for this job, so the reason has nowhere to live")
}

// The brief-wide row logs the defect at the level the SYNCHRONOUS paths use, not below it.
//
// This endpoint returns a SUCCESSFUL aggregate: the row carries status `failed` and the
// request is a 200. So unlike every other consumer of this sentinel there is no status code
// carrying the alarm, and the log line is the entire signal that a defect in this service
// occurred. GetCampaignMetrics, ToggleCampaignStatus and the discovery handler all answer 500
// AND log at ERROR for this same sentinel; if the fan-out row logs it at WARN the identical
// defect is visible at two different levels, and at the one below the threshold anybody
// watches — while the caller is told the request succeeded.
//
// The level is asserted directly rather than through the presence of a line: a WARN and an
// ERROR are the same record with the same message, so a test that only proved "something was
// logged" would pass against the defect this pins.
func TestServiceDefect_BriefMetricsRowLogsAtErrorNotWarn(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	disp := newPerCampaignDispatcher()
	disp.errs["c1"] = serviceDefectErr()
	s := newBriefMetricsService(t, disp, campaignOn("c1", model.ProviderLinkedInAds))

	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{
		ProjectID: "cncf", BriefID: "b1",
	})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	// Bind the fixture to the classification first. If the dispatcher error stopped reaching
	// classifyBriefMetricsErr at all, every level assertion below would pass vacuously
	// against a row that never took the defect arm.
	if got := rowByCampaign(t, res, "c1").Status; got != "failed" {
		t.Fatalf("row status = %q, want \"failed\" — the fixture is not reaching the "+
			"ErrServiceDefect arm, so the level assertion below would prove nothing", got)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	var found bool
	for _, rec := range h.recs {
		var campaign string
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "campaign_id" {
				campaign = a.Value.String()
			}
			return true
		})
		// slog.Default is process-wide; a sibling test's fan-out can drain here.
		if campaign != "c1" || !strings.Contains(rec.Message, "brief metrics row could not be read") {
			continue
		}
		found = true
		if rec.Level != slog.LevelError {
			t.Errorf("the brief-wide row logged this service's own defect at %v, want ERROR — "+
				"the aggregate answers 200, so this line is the ONLY signal the defect "+
				"occurred, and the synchronous paths log the same sentinel at ERROR", rec.Level)
		}
	}
	if !found {
		t.Fatal("no per-row log line for campaign c1; the level assertion proved nothing")
	}
}
