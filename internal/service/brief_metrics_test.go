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
	"math"
	"strings"
	"sync"
	"testing"
	"time"

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
			wantStatus: "connection_problem", wantReason: "could not be decrypted",
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

// A fixed instant for the pacing cases. Date arithmetic tested against the wall clock passes or
// fails by WHEN it is run.
var metricsNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func budgeted(c *model.Campaign, amount float64, kind model.BudgetType, startOffset, endOffset int) *model.Campaign {
	start := metricsNow.AddDate(0, 0, startOffset)
	end := metricsNow.AddDate(0, 0, endOffset)
	c.BudgetAmount = &amount
	c.BudgetType = &kind
	c.StartDate = &start
	c.EndDate = &end
	return c
}

// Pacing is spend against the flight-prorated plan, and it must be computed over the period the
// SPEND covers rather than the elapsed flight.
func TestGetBriefMetrics_PacingIsProratedAcrossTheFlight(t *testing.T) {
	// Day 10 of a 20-day $1000 flight. The default window is 30 days, capped to the 10 elapsed,
	// so expected-by-now is $500. Spending exactly that is on plan.
	c := budgeted(campaignOn("c1", model.ProviderGoogleAds), 1000, model.BudgetLifetime, -10, 10)
	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 10000, Clicks: 200, CostMicros: 500_000_000, Ctr: 0.02}

	s := newBriefMetricsService(t, disp, c)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	row := rowByCampaign(t, res, "c1")
	if row.Pacing == nil {
		t.Fatal("a measured row with a budget and a flight carries no pacing")
	}
	if row.Pacing.Pct == nil {
		t.Fatal("computable pacing carries no pct")
	}
	if math.Abs(*row.Pacing.Pct-100) > 5 {
		t.Errorf("pacing = %.1f%%, want ~100%% (half the budget, half way through)", *row.Pacing.Pct)
	}
	if row.Pacing.Label != "normal" {
		t.Errorf("label = %q, want normal", row.Pacing.Label)
	}
	// On plan, healthy CTR, spending: nothing to flag.
	if len(res.ActionItems) != 0 {
		t.Errorf("a healthy campaign raised %d action items", len(res.ActionItems))
	}
}

// A campaign with no budget has no pacing, and the absence must be visible as `unknown` with an
// ABSENT pct — not as 0%, which reads as a campaign that spent nothing.
func TestGetBriefMetrics_NoBudgetYieldsUnknownPacingNotZero(t *testing.T) {
	c := campaignOn("c1", model.ProviderGoogleAds) // no BudgetAmount
	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 10000, Clicks: 200, CostMicros: 5_000_000, Ctr: 0.02}

	s := newBriefMetricsService(t, disp, c)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	row := rowByCampaign(t, res, "c1")
	if row.Pacing == nil {
		t.Fatal("an ok row carries no pacing object at all; unknown must be stated, not omitted")
	}
	if row.Pacing.Label != "unknown" {
		t.Errorf("label = %q, want unknown", row.Pacing.Label)
	}
	if row.Pacing.Pct != nil {
		t.Errorf("incomputable pacing carries pct = %v; it must be ABSENT, because 0%% is a claim about spend", *row.Pacing.Pct)
	}
	for _, item := range res.ActionItems {
		if item.Rule == "underspending" || item.Rule == "budget_constrained" {
			t.Errorf("%s raised against a campaign with no budget — that is the absence of a plan, not a spend finding", item.Rule)
		}
	}
}

// A row that could not be read must raise NO action items. Evaluating it would see zero
// impressions and zero spend and report every failed read as a dead campaign.
func TestGetBriefMetrics_UnreadableRowRaisesNoActionItems(t *testing.T) {
	bad := budgeted(campaignOn("c2", model.ProviderMetaAds), 1000, model.BudgetLifetime, -10, 10)
	disp := newPerCampaignDispatcher()
	disp.errs["c2"] = errors.New("meta graph api: 500 internal error")

	s := newBriefMetricsService(t, disp, bad)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	if len(res.ActionItems) != 0 {
		t.Errorf("an unreadable row raised %d action items (%+v) — that reports an outage as a campaign defect", len(res.ActionItems), res.ActionItems)
	}
	if row := rowByCampaign(t, res, "c2"); row.Pacing != nil {
		t.Errorf("a row with no measurement carries pacing %+v", row.Pacing)
	}
	// And the list is [] rather than null, so a consumer can iterate it unconditionally.
	if res.ActionItems == nil {
		t.Error("action_items is nil; it must marshal as [] so empty is distinguishable from absent")
	}
}

// The rules see a CTR in PERCENT. The domain model carries a ratio, so a missing conversion
// makes the threshold a hundred times too strict and flags every healthy campaign.
func TestGetBriefMetrics_LowCTRUsesPercentNotRatio(t *testing.T) {
	healthy := budgeted(campaignOn("c1", model.ProviderGoogleAds), 1000, model.BudgetLifetime, -10, 10)
	poor := budgeted(campaignOn("c2", model.ProviderLinkedInAds), 1000, model.BudgetLifetime, -10, 10)
	disp := newPerCampaignDispatcher()
	// 2% CTR — comfortably healthy. As a raw ratio (0.02) it would sit below the 0.3 threshold
	// and be flagged.
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 20000, Clicks: 400, CostMicros: 500_000_000, Ctr: 0.02}
	// 0.1% CTR — genuinely poor.
	disp.results["c2"] = &model.CampaignMetrics{Impressions: 20000, Clicks: 20, CostMicros: 500_000_000, Ctr: 0.001}

	s := newBriefMetricsService(t, disp, healthy, poor)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	var flagged []string
	for _, item := range res.ActionItems {
		if item.Rule == "low_ctr" {
			flagged = append(flagged, item.CampaignID)
		}
	}
	if len(flagged) != 1 || flagged[0] != "c2" {
		t.Errorf("low_ctr flagged %v, want [c2] only — a 2%% CTR is healthy and must not fire", flagged)
	}
}

// The window the row was READ over is what pacing must be computed against. Spend from a 7-day
// window compared to 30 days of plan reports an on-track campaign as spending a fifth of what
// it should — a confident figure about a period nobody asked about.
func TestGetBriefMetrics_PacingUsesTheRowsOwnWindow(t *testing.T) {
	// Day 20 of a 40-day $4000 flight: $100/day of plan. Exactly on plan over 7 days = $700.
	c := budgeted(campaignOn("c1", model.ProviderGoogleAds), 4000, model.BudgetLifetime, -20, 20)
	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 20000, Clicks: 400, CostMicros: 700_000_000, Ctr: 0.02}

	s := newBriefMetricsService(t, disp, c)
	s.SetClock(func() time.Time { return metricsNow })
	window := string(model.MetricsWindowLast7Days)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1", Window: &window})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	row := rowByCampaign(t, res, "c1")
	if row.Pacing == nil || row.Pacing.Pct == nil {
		t.Fatal("no computable pacing")
	}
	// ~108%, not 100%: the window is clamped to now, so `last_7_days` at noon contributes 6.5
	// days of plan ($650) against the 7 days of spend the platform reports ($700). That bias is
	// deliberate and bounded — see the clamp's comment in window.go, where erring toward
	// "spending ahead" is the safer direction because it never manufactures an underspending
	// item against a healthy campaign.
	//
	// What this test is really about is the DENOMINATOR's window: against the 30-day default it
	// would be ~23% and raise an underspending item against a campaign that is exactly on track.
	// 108 vs 23 proves the row's own window was used; the exact figure is asserted so a later
	// change to the clamp cannot pass unnoticed.
	if math.Abs(*row.Pacing.Pct-107.7) > 1 {
		t.Errorf("pacing = %.1f%%, want ~107.7%% — expected spend must cover the 7-day window this row was read over, clamped to now", *row.Pacing.Pct)
	}
	for _, item := range res.ActionItems {
		if item.Rule == "underspending" {
			t.Error("underspending raised against a campaign that is exactly on plan for its window")
		}
	}
}

// A campaign with a budget but NO start date must not be paced.
//
// start_date is nullable in the schema, so this is a storable state. Defaulting the start to
// `now` makes the flight begin this instant and the one-day elapsed floor then compares a
// 30-day window of spend against a single day of plan: a campaign exactly on plan reports
// 500% overspending and raises a budget item against itself.
func TestGetBriefMetrics_CampaignWithNoStartDateIsNotPaced(t *testing.T) {
	c := campaignOn("c1", model.ProviderGoogleAds)
	amount, kind := 1000.0, model.BudgetLifetime
	end := metricsNow.AddDate(0, 0, 10)
	c.BudgetAmount = &amount
	c.BudgetType = &kind
	c.EndDate = &end // StartDate deliberately left nil

	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 20000, Clicks: 400, CostMicros: 500_000_000, Ctr: 0.02}

	s := newBriefMetricsService(t, disp, c)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	row := rowByCampaign(t, res, "c1")
	if row.Pacing == nil {
		t.Fatal("an ok row must state pacing, even when it is unknown")
	}
	if row.Pacing.Label != "unknown" {
		t.Errorf("label = %q, want unknown — there is no flight to prorate across", row.Pacing.Label)
	}
	if row.Pacing.Pct != nil {
		t.Errorf("pct = %v; a campaign with no start date has no plan-to-date to measure against", *row.Pacing.Pct)
	}
	for _, item := range res.ActionItems {
		if item.Rule == "budget_constrained" || item.Rule == "underspending" {
			t.Errorf("%s raised against a campaign with no recorded start: %q", item.Rule, item.Issue)
		}
	}
}

func TestGetBriefMetrics_ConnectionFailureLogsNoErrorText(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	secret := "AKIAIOSFODNN7EXAMPLE-decrypted-blob-fragment"
	disp := newPerCampaignDispatcher()
	disp.errs["c1"] = fmt.Errorf("%w: %s", domain.ErrConnectionNotUsable, secret)

	s := newBriefMetricsService(t, disp, campaignOn("c1", model.ProviderGoogleAds))
	if _, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"}); err != nil {
		t.Fatalf("a connection failure must not fail the request: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, secret) {
		t.Errorf("the wrapped cause reached the log sink:\n%s", logged)
	}
	// The line must still exist and still say WHY, or the assertion above would pass simply by
	// logging nothing at all.
	if !strings.Contains(logged, "brief metrics row could not be read") {
		t.Fatalf("no log line was written for the failed row:\n%s", logged)
	}
	if !strings.Contains(logged, "reason=") {
		t.Errorf("the connection arm logged no reason token, so an operator cannot tell what to fix:\n%s", logged)
	}
}

func TestGetBriefMetrics_DecryptFailureLogsNoCauseText(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	secret := "ciphertext-0xDEADBEEF-key-material"
	disp := newPerCampaignDispatcher()
	disp.errs["c1"] = fmt.Errorf("%w: %s", domain.ErrCredentialDecryptionFailed, secret)

	s := newBriefMetricsService(t, disp, campaignOn("c1", model.ProviderGoogleAds))
	if _, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"}); err != nil {
		t.Fatalf("a decrypt failure must not fail the whole request: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, secret) {
		t.Errorf("the encryptor's cause reached the log sink:\n%s", logged)
	}
	if strings.Contains(logged, "error=") {
		t.Errorf("the decrypt arm logged an error field; it must log none:\n%s", logged)
	}
	// A rotated key fails every project at once and the cheap discriminator is the COUNT of
	// these lines, so the line itself must exist and be at ERROR — aggregated into per-row
	// WARNs it would page nobody.
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("a decrypt failure was not logged at ERROR:\n%s", logged)
	}
}

// A window that precedes the campaign's flight must not be paced at the ENDPOINT either.
//
// The overlap fix lives in the rules package, but the endpoint chooses which figure to pass. This
// pins the wiring: passing the window's bare LENGTH instead of its overlap with the flight makes
// this test fail, which is what stops the fix regressing at the caller while the unit tests stay
// green.
func TestGetBriefMetrics_WindowPrecedingTheFlightIsNotPaced(t *testing.T) {
	// Flight begins on the 13th of the current month; `last_month` lies entirely before it.
	c := campaignOn("c1", model.ProviderGoogleAds)
	amount, kind := 1000.0, model.BudgetLifetime
	start := time.Date(metricsNow.Year(), metricsNow.Month(), 13, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 30)
	c.BudgetAmount = &amount
	c.BudgetType = &kind
	c.StartDate = &start
	c.EndDate = &end

	disp := newPerCampaignDispatcher()
	// Zero spend is CORRECT for this window — the campaign did not exist during it.
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 0, Clicks: 0, CostMicros: 0, Ctr: 0}

	s := newBriefMetricsService(t, disp, c)
	s.SetClock(func() time.Time { return metricsNow })
	window := string(model.MetricsWindowLastMonth)
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", Window: &window,
	})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	row := rowByCampaign(t, res, "c1")
	if row.Pacing == nil {
		t.Fatal("an ok row must state pacing even when unknown")
	}
	if row.Pacing.Label != "unknown" {
		t.Errorf("label = %q, want unknown — the window precedes the flight entirely", row.Pacing.Label)
	}
	if row.Pacing.Pct != nil {
		t.Errorf("pct = %v; the campaign did not exist during this window", *row.Pacing.Pct)
	}
	for _, item := range res.ActionItems {
		if item.Rule == "underspending" {
			t.Errorf("underspending raised for a window that precedes the campaign: %q", item.Issue)
		}
	}
}

// An email send with no opens must not be reported as a campaign that never ran.
//
// HubSpot maps opens onto Impressions and always reports CostMicros=0, so a delivered email
// nobody opened is numerically identical to a paid campaign that never served. This pins the
// WIRING: hardcoding BillsPerDelivery at the caller makes this test fail, which is what stops the
// channel distinction regressing while the rules-package tests stay green.
func TestGetBriefMetrics_EmailWithNoOpensIsNotZeroDelivery(t *testing.T) {
	email := campaignOn("c1", model.ProviderHubSpot)
	disp := newPerCampaignDispatcher()
	// Delivered, but nobody opened: zero "impressions", zero cost — and correct.
	disp.results["c1"] = &model.CampaignMetrics{
		Impressions: 0, Clicks: 0, CostMicros: 0, Ctr: 0,
		Email: &model.EmailMetrics{Sent: 500, Delivered: 495},
	}

	s := newBriefMetricsService(t, disp, email)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}

	for _, item := range res.ActionItems {
		if item.Rule == "zero_delivery" {
			t.Errorf("zero_delivery raised for an email delivered to 495 recipients: %q", item.Issue)
		}
	}

	// A PAID campaign with the identical numbers still fires, or the assertion above would pass
	// for any reason at all.
	paid := campaignOn("c2", model.ProviderGoogleAds)
	disp2 := newPerCampaignDispatcher()
	disp2.results["c2"] = &model.CampaignMetrics{Impressions: 0, Clicks: 0, CostMicros: 0, Ctr: 0}
	s2 := newBriefMetricsService(t, disp2, paid)
	s2.SetClock(func() time.Time { return metricsNow })
	res2, err := s2.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics (paid): %v", err)
	}
	var found bool
	for _, item := range res2.ActionItems {
		if item.Rule == "zero_delivery" {
			found = true
		}
	}
	if !found {
		t.Error("zero_delivery did not fire for a paid campaign with no delivery")
	}
}

// A campaign dispatched before its flight begins must not be reported as failing to deliver.
//
// Pins the WIRING, not just the rule: hardcoding DeliveryExpected at the caller makes this fail.
// The caller half of this gate went untested twice already on this branch.
func TestGetBriefMetrics_CampaignBeforeItsFlightIsNotZeroDelivery(t *testing.T) {
	// Dispatched now, scheduled to start in a week.
	c := campaignOn("c1", model.ProviderGoogleAds)
	amount, kind := 1000.0, model.BudgetLifetime
	start := metricsNow.AddDate(0, 0, 7)
	end := start.AddDate(0, 0, 30)
	c.BudgetAmount, c.BudgetType, c.StartDate, c.EndDate = &amount, &kind, &start, &end

	disp := newPerCampaignDispatcher()
	disp.results["c1"] = &model.CampaignMetrics{Impressions: 0, Clicks: 0, CostMicros: 0, Ctr: 0}

	s := newBriefMetricsService(t, disp, c)
	s.SetClock(func() time.Time { return metricsNow })
	res, err := s.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics: %v", err)
	}
	for _, item := range res.ActionItems {
		if item.Rule == "zero_delivery" {
			t.Errorf("zero_delivery raised for a campaign that starts in a week: %q", item.Issue)
		}
	}

	// A campaign whose flight started a week ago, same zero metrics, DOES fire — or the
	// assertion above would pass for any reason at all.
	started := campaignOn("c2", model.ProviderGoogleAds)
	s2Start := metricsNow.AddDate(0, 0, -7)
	s2End := metricsNow.AddDate(0, 0, 23)
	started.BudgetAmount, started.BudgetType, started.StartDate, started.EndDate = &amount, &kind, &s2Start, &s2End
	disp2 := newPerCampaignDispatcher()
	disp2.results["c2"] = &model.CampaignMetrics{Impressions: 0, Clicks: 0, CostMicros: 0, Ctr: 0}
	s2 := newBriefMetricsService(t, disp2, started)
	s2.SetClock(func() time.Time { return metricsNow })
	res2, err := s2.GetBriefMetrics(context.Background(), &briefs.GetBriefMetricsPayload{ProjectID: "cncf", BriefID: "b1"})
	if err != nil {
		t.Fatalf("GetBriefMetrics (started): %v", err)
	}
	var found bool
	for _, item := range res2.ActionItems {
		if item.Rule == "zero_delivery" {
			found = true
		}
	}
	if !found {
		t.Error("zero_delivery did not fire for a campaign a week into its flight with no delivery")
	}
}
