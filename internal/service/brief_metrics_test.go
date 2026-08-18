// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// perCampaignDispatcher answers ReadMetrics differently per campaign id, and is safe to call
// from the concurrent fan-out GetBriefMetrics performs.
//
// metricsOnlyDispatcher cannot be reused here: it stores one result for the whole dispatcher
// and writes gotWindow without a mutex, so a brief-wide read would both race on that field
// and be unable to give two campaigns on the same platform different outcomes — which is
// exactly what these tests need to distinguish.
type perCampaignDispatcher struct {
	mu      sync.Mutex
	results map[string]*model.CampaignMetrics
	errs    map[string]error
	// windows records the window each campaign was actually read over, so a test can prove
	// the per-platform default was applied rather than the request-level one.
	windows map[string]model.MetricsWindow
	// calls counts reads, so a test can prove every campaign was attempted — the property a
	// cancelled errgroup would silently break.
	calls int
}

func newPerCampaignDispatcher() *perCampaignDispatcher {
	return &perCampaignDispatcher{
		results: map[string]*model.CampaignMetrics{},
		errs:    map[string]error{},
		windows: map[string]model.MetricsWindow{},
	}
}

func (*perCampaignDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch should not be called in these tests")
}

func (d *perCampaignDispatcher) ReadMetrics(ctx context.Context, _ string, _ model.Provider, c *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	d.mu.Lock()
	d.calls++
	d.windows[c.ID] = window
	err, hasErr := d.errs[c.ID]
	res := d.results[c.ID]
	d.mu.Unlock()
	// Honour cancellation so a test can observe whether one row's failure tore down the
	// others. Without this the fan-out would complete regardless and the assertion that
	// every campaign was attempted would pass vacuously.
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if hasErr {
		return nil, err
	}
	return res, nil
}

func (d *perCampaignDispatcher) windowFor(id string) model.MetricsWindow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.windows[id]
}

func (d *perCampaignDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// newBriefMetricsService wires a service whose brief b1 carries the given campaigns, with one
// dispatcher shared across every platform they use.
func newBriefMetricsService(t *testing.T, disp PlatformDispatcher, camps ...*model.Campaign) *BriefService {
	t.Helper()
	repo := newFakeBriefRepo()
	// The handler reads the brief FIRST so a missing or archived brief is a 404 for the
	// request rather than an empty row set that reads as "this brief has no campaigns".
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: "kubecon-eu-2026", Version: 1,
	}
	byID := map[string]*model.Campaign{}
	dispatchers := map[model.Provider]PlatformDispatcher{}
	for _, c := range camps {
		byID[c.ID] = c
		dispatchers[c.Platform] = disp
	}
	cr := &fakeCampaignRepo{byID: byID}
	jobs := newFakeJobRepo()
	return NewBriefService(repo, cr, jobs, NewOrchestrator(cr, jobs, dispatchers))
}

func campaignOn(id string, platform model.Provider) *model.Campaign {
	return &model.Campaign{
		ID: id, ProjectID: "cncf", BriefID: "b1", Platform: platform,
		PlatformCampaignID: id + "-upstream", Status: model.CampaignStatusCreated, Version: 1,
	}
}

func rowByCampaign(t *testing.T, res *briefs.BriefMetrics, id string) *briefs.BriefMetricsRow {
	t.Helper()
	for _, r := range res.Rows {
		if r.CampaignID == id {
			return r
		}
	}
	t.Fatalf("no row for campaign %q; got %d rows", id, len(res.Rows))
	return nil
}

// A failed row must carry NO metrics. This is the whole reason the aggregate exists: a
// zero-filled row is indistinguishable from a campaign that genuinely served nothing, and
// substituting one for an unreadable campaign is what turns an outage into an apparent
// performance result — the defect that produced a false "pause losing campaigns"
// recommendation on the ED dashboard.
func TestGetBriefMetrics_FailedRowCarriesNoMetricsRatherThanZeroes(t *testing.T) {
	ok := campaignOn("c1", model.ProviderGoogleAds)
	bad := campaignOn("c2", model.ProviderMetaAds)
	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 100, Clicks: 10, CostMicros: 5_000_000, Ctr: 0.1}
	disp.errs["c2"] = errors.New("meta graph api: 500 internal error")

	s := newBriefMetricsService(t, disp, ok, bad)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	failed := rowByCampaign(t, res, "c2")
	if failed.Status != "failed" {
		t.Errorf("status = %q, want failed", failed.Status)
	}
	if failed.Metrics != nil {
		t.Errorf("a row that could not be read carries metrics %+v — a zero is a claim, and this row measured nothing", failed.Metrics)
	}
	if failed.Reason == nil || *failed.Reason == "" {
		t.Error("a non-ok row must say why it carries no measurement")
	}
	// The healthy row is unaffected, and still carries real numbers.
	good := rowByCampaign(t, res, "c1")
	if good.Status != "ok" || good.Metrics == nil || good.Metrics.Impressions != 100 {
		t.Errorf("healthy row = %+v (metrics %+v)", good, good.Metrics)
	}
	if res.OKCount != 1 {
		t.Errorf("ok_count = %d, want 1 — it is what tells a consumer a total covers 1 of 2 campaigns", res.OKCount)
	}
	if len(res.Rows) != 2 {
		t.Errorf("got %d rows, want one per campaign INCLUDING the unreadable one", len(res.Rows))
	}
}

// One platform's failure must not prevent the others from being READ AT ALL.
//
// The mechanism this guards: every g.Go returns nil deliberately. Returning the error would
// cancel the errgroup's context, and campaigns whose reads had not yet started would be
// abandoned — reported as failures the service never actually attempted. A consumer cannot
// tell that apart from four genuinely broken platforms.
func TestGetBriefMetrics_OneFailureDoesNotAbandonTheOtherCampaigns(t *testing.T) {
	camps := []*model.Campaign{
		campaignOn("c1", model.ProviderGoogleAds),
		campaignOn("c2", model.ProviderMetaAds),
		campaignOn("c3", model.ProviderLinkedInAds),
		campaignOn("c4", model.ProviderRedditAds),
	}
	disp := newPerCampaignDispatcher()
	// The FIRST campaign in sort order fails, so a cancelling implementation would tear down
	// the rest before they ran.
	disp.errs["c1"] = errors.New("google ads: 503 service unavailable")
	for _, id := range []string{"c2", "c3", "c4"} {
		disp.results[id] = &model.CampaignMetrics{Impressions: 42, Clicks: 4, Ctr: 0.095}
	}

	s := newBriefMetricsService(t, disp, camps...)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	if got := disp.callCount(); got != 4 {
		t.Fatalf("the platform was read %d times, want 4 — a cancelled fan-out abandons campaigns and reports failures it never attempted", got)
	}
	if res.OKCount != 3 {
		t.Errorf("ok_count = %d, want 3", res.OKCount)
	}
	for _, id := range []string{"c2", "c3", "c4"} {
		r := rowByCampaign(t, res, id)
		if r.Status != "ok" || r.Metrics == nil {
			t.Errorf("campaign %s: status=%q metrics=%v — a sibling's failure must not affect it", id, r.Status, r.Metrics)
		}
	}
}

// Each sentinel maps to the status whose ADVICE is right for it. A wrong mapping sends an
// operator to retry something permanent, or to investigate a healthy email draft as an outage.
func TestGetBriefMetrics_SentinelsMapToTheirRowStatus(t *testing.T) {
	cases := map[string]struct {
		err        error
		platform   model.Provider
		wantStatus string
		wantReason string
	}{
		"platform has no metrics reader": {
			err: domain.ErrMetricsUnsupported, platform: model.ProviderRedditAds,
			wantStatus: "unsupported", wantReason: "not supported",
		},
		"window too wide for the platform": {
			err: domain.ErrMetricsWindowUnsupported, platform: model.ProviderTwitterAds,
			wantStatus: "unsupported", wantReason: "7-day queryable range",
		},
		"campaign never reached the platform": {
			err: domain.ErrCampaignNotProvisioned, platform: model.ProviderGoogleAds,
			wantStatus: "not_ready", wantReason: "no platform campaign id",
		},
		"staged email draft, nothing sent yet": {
			err: domain.ErrNoMetricsInWindow, platform: model.ProviderHubSpot,
			wantStatus: "not_ready", wantReason: "no data for this campaign",
		},
		"campaign predates provenance tracking": {
			err: domain.ErrCampaignProvenanceUnknown, platform: model.ProviderMetaAds,
			wantStatus: "connection_problem", wantReason: "re-dispatched",
		},
		"connection now points at another account": {
			err: domain.ErrCampaignAccountMismatch, platform: model.ProviderMetaAds,
			wantStatus: "connection_problem", wantReason: "different account",
		},
		// wantReason is deliberately a phrase the GENERAL connection arm does not contain:
		// "not usable" appears in both, so it passed whether or not the system case had its
		// own arm — and the general wording tells a project with no connection of its own to
		// "reconnect it", which it cannot do.
		"no ad account chosen on the connection": {
			err: domain.ErrAccountNotSelected, platform: model.ProviderGoogleAds,
			wantStatus: "connection_problem", wantReason: "no ad account has been selected",
		},
		"system fallback connection unusable": {
			err: domain.ErrSystemConnectionNotUsable, platform: model.ProviderLinkedInAds,
			wantStatus: "connection_problem", wantReason: "shared LF connection",
		},
		"the project's own connection is unusable": {
			err: domain.ErrConnectionNotUsable, platform: model.ProviderMetaAds,
			wantStatus: "connection_problem", wantReason: "reconnect it",
		},
		"stored credentials cannot be decrypted": {
			err: domain.ErrCredentialDecryptionFailed, platform: model.ProviderGoogleAds,
			wantStatus: "connection_problem", wantReason: "could not be read",
		},
		"no connection row for this project": {
			err: domain.ErrNotFound, platform: model.ProviderRedditAds,
			wantStatus: "connection_problem", wantReason: "no connection",
		},
		"unrecognised failure defaults to retryable": {
			err: errors.New("connection reset by peer"), platform: model.ProviderGoogleAds,
			wantStatus: "failed", wantReason: "could not be reached",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Wrapped, not bare: the service sees adapter-wrapped errors in production, and a
			// classifier written with == instead of errors.Is would pass on a bare sentinel.
			gotStatus, gotReason := classifyBriefMetricsErr(fmt.Errorf("adapter: %w", tc.err), tc.platform)
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tc.wantStatus)
			}
			if !contains(gotReason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", gotReason, tc.wantReason)
			}
		})
	}
}

// not_ready and failed must stay DISTINCT. A staged email draft and an ad-platform outage
// both produce no numbers and want opposite responses — one is the expected steady state
// before a human presses send, the other is an incident.
func TestGetBriefMetrics_StagedEmailIsNotReportedAsAnOutage(t *testing.T) {
	email := campaignOn("c1", model.ProviderHubSpot)
	broken := campaignOn("c2", model.ProviderGoogleAds)
	disp := newPerCampaignDispatcher()
	disp.errs["c1"] = fmt.Errorf("hubspot: %w", domain.ErrNoMetricsInWindow)
	disp.errs["c2"] = errors.New("google ads: 502 bad gateway")

	s := newBriefMetricsService(t, disp, email, broken)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	if got := rowByCampaign(t, res, "c1").Status; got != "not_ready" {
		t.Errorf("a staged email draft reported as %q — that sends an operator to investigate a healthy integration", got)
	}
	if got := rowByCampaign(t, res, "c2").Status; got != "failed" {
		t.Errorf("an ad-platform outage reported as %q — retrying is the right advice and only `failed` gives it", got)
	}
}

// The adapter's own error text must never reach the consumer: it can carry a platform's
// response body or operator-supplied account identifiers.
func TestGetBriefMetrics_ReasonDoesNotLeakAdapterErrorText(t *testing.T) {
	const secret = "act_998877665544 token=sk-live-abc123"
	camp := campaignOn("c1", model.ProviderMetaAds)
	disp := newPerCampaignDispatcher()
	disp.errs["c1"] = fmt.Errorf("meta graph api rejected %s", secret)

	s := newBriefMetricsService(t, disp, camp)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	row := rowByCampaign(t, res, "c1")
	if row.Reason == nil {
		t.Fatal("no reason on a failed row")
	}
	if contains(*row.Reason, "act_998877665544") || contains(*row.Reason, "sk-live-abc123") {
		t.Errorf("reason leaked adapter error text: %q", *row.Reason)
	}
}

// With no request-level window each row is read over its PLATFORM's default, which is not the
// same for every platform: X Ads caps queryable ranges at 7 days, so last_30_days is
// unreachable there. The top-level window must not claim otherwise for rows it does not cover.
func TestGetBriefMetrics_PerPlatformWindowDefaultsAreApplied(t *testing.T) {
	google := campaignOn("c1", model.ProviderGoogleAds)
	x := campaignOn("c2", model.ProviderTwitterAds)
	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 10}
	disp.results["c2"] = &model.CampaignMetrics{Impressions: 20}

	s := newBriefMetricsService(t, disp, google, x)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	gw, xw := disp.windowFor("c1"), disp.windowFor("c2")
	if gw == xw {
		t.Fatalf("both platforms read over %q — X Ads cannot serve the 30-day default and must fall back", gw)
	}
	if gw != model.MetricsWindowLast30Days {
		t.Errorf("google ads read over %q, want last_30_days", gw)
	}
	// Each row reports the window IT was read over, not the top-level one.
	if got := rowByCampaign(t, res, "c2").Metrics.Window; got != string(xw) {
		t.Errorf("X row reports window %q but was read over %q", got, xw)
	}
}

// A brief with no campaigns is an ordinary state, not an error: it is what every brief looks
// like before it is dispatched.
func TestGetBriefMetrics_BriefWithNoCampaignsIsNotAnError(t *testing.T) {
	s := newBriefMetricsService(t, newPerCampaignDispatcher())
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("an undispatched brief must read as empty, not fail: %v", err)
	}
	if len(res.Rows) != 0 || res.OKCount != 0 {
		t.Errorf("rows=%d ok_count=%d, want both zero", len(res.Rows), res.OKCount)
	}
}

// An invalid window is refused for the whole REQUEST rather than becoming eight identical
// per-row failures — the caller's mistake is not a property of any campaign.
func TestGetBriefMetrics_InvalidWindowIsRefusedUpFront(t *testing.T) {
	disp := newPerCampaignDispatcher()
	s := newBriefMetricsService(t, disp, campaignOn("c1", model.ProviderGoogleAds))
	bad := "last_45_days"
	_, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", Window: &bad,
	})
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("want a 400 BadRequestError, got %T: %v", err, err)
	}
	if disp.callCount() != 0 {
		t.Error("the platform was contacted despite an invalid window")
	}
}

// A campaign belonging to another project must not appear, even when the brief id collides.
//
// The real query filters on BOTH brief_id and project_id; a fake that filtered on brief_id
// alone would let the project_id predicate be deleted from the SQL with nothing failing —
// exactly the "reverting a fix changes no test" shape. This test is what makes the fake's
// tenant filter load-bearing rather than decorative.
func TestGetBriefMetrics_ExcludesAnotherProjectsCampaign(t *testing.T) {
	mine := campaignOn("c1", model.ProviderGoogleAds)
	theirs := campaignOn("c2", model.ProviderMetaAds)
	theirs.ProjectID = "some-other-foundation" // same brief id, different tenant

	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 10}
	disp.results["c2"] = &model.CampaignMetrics{Impressions: 999}

	s := newBriefMetricsService(t, disp, mine, theirs)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	for _, r := range res.Rows {
		if r.CampaignID == "c2" {
			t.Error("another project's campaign appeared in this project's brief metrics")
		}
	}
	if len(res.Rows) != 1 {
		t.Errorf("got %d rows, want 1 — only this project's campaign", len(res.Rows))
	}
}

// A missing or archived brief is a request-level 404, NOT an empty row set. The distinction is
// the same one this endpoint exists to make: "this brief has no campaigns" and "there is no such
// brief" are different answers, and rows=[] with ok_count=0 asserts the first.
//
// Also asserts nothing downstream ran: the guard must refuse BEFORE listing campaigns or
// contacting any platform.
func TestGetBriefMetrics_MissingBriefIs404WithNoDownstreamCalls(t *testing.T) {
	disp := newPerCampaignDispatcher()
	s := newBriefMetricsService(t, disp, campaignOn("c1", model.ProviderGoogleAds))

	_, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{
		ProjectID: "cncf", BriefID: "no-such-brief",
	})

	var notFound *briefs.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("want a 404 NotFoundError, got %T: %v", err, err)
	}
	if disp.callCount() != 0 {
		t.Errorf("the platform was contacted %d times for a brief that does not exist", disp.callCount())
	}
}

// An ARCHIVED brief is unreadable for the same reason a missing one is — GetBrief refuses it —
// so it must not degrade into an empty row set either.
func TestGetBriefMetrics_ArchivedBriefIs404(t *testing.T) {
	disp := newPerCampaignDispatcher()
	repo := newFakeBriefRepo()
	// The real GetBrief filters `status <> 'archived'` (brief_repo.go:68), so an archived brief
	// reads as ErrNotFound. The fake keys on presence alone, so the archived case is modelled
	// the way the repository ANSWERS it rather than by storing a status the fake would ignore.
	repo.getErr = domain.ErrNotFound
	cr := &fakeCampaignRepo{byID: map[string]*model.Campaign{}}
	jobs := newFakeJobRepo()
	s := NewBriefService(repo, cr, jobs, NewOrchestrator(cr, jobs,
		map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp}))

	_, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{
		ProjectID: "cncf", BriefID: "b1",
	})
	var notFound *briefs.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("an archived brief must be unreadable, got %T: %v", err, err)
	}
	if disp.callCount() != 0 {
		t.Errorf("the platform was contacted %d times for an archived brief", disp.callCount())
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
