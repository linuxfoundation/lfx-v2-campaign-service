// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// recordingMetrics captures what the orchestrator records. Guarded by a mutex
// because dispatch records from one goroutine per platform, and the suite runs
// under -race.
type recordingMetrics struct {
	mu        sync.Mutex
	dispatch  []dispatchRecord
	jobStates []model.JobStatus
	upstream  []upstreamRecord
}

type dispatchRecord struct {
	platform model.Provider
	outcome  string
}

type upstreamRecord struct {
	platform  model.Provider
	operation string
	outcome   string
	seconds   float64
}

func (m *recordingMetrics) RecordDispatch(_ context.Context, p model.Provider, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatch = append(m.dispatch, dispatchRecord{platform: p, outcome: outcome})
}

func (m *recordingMetrics) RecordJobTransition(_ context.Context, s model.JobStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobStates = append(m.jobStates, s)
}

func (m *recordingMetrics) RecordUpstreamCall(_ context.Context, p model.Provider, operation, outcome string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstream = append(m.upstream, upstreamRecord{platform: p, operation: operation, outcome: outcome, seconds: seconds})
}

func (m *recordingMetrics) outcomesFor(p model.Provider) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, d := range m.dispatch {
		if d.platform == p {
			out = append(out, d.outcome)
		}
	}
	return out
}

// upstreamCalls returns a copy of the recorded upstream calls.
func (m *recordingMetrics) upstreamCalls() []upstreamRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]upstreamRecord(nil), m.upstream...)
}

func (m *recordingMetrics) sawJobState(s model.JobStatus) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, got := range m.jobStates {
		if got == s {
			return true
		}
	}
	return false
}

// TestDispatchMetricsRecordSuccessAndFailure pins that each platform's dispatch
// outcome is recorded, with success and failure distinguished. Without this, a
// dashboard would show dispatch volume but not whether the campaigns landed.
func TestDispatchMetricsRecordSuccessAndFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	rec := &recordingMetrics{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: failDispatcher{},
	})
	orch.SetMetrics(rec)

	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, jobs, id)

	if got := rec.outcomesFor(model.ProviderGoogleAds); len(got) != 1 || got[0] != dispatchOutcomeSuccess {
		t.Errorf("google-ads outcomes = %v, want [%s]", got, dispatchOutcomeSuccess)
	}
	if got := rec.outcomesFor(model.ProviderLinkedInAds); len(got) != 1 || got[0] != dispatchOutcomeFailure {
		t.Errorf("linkedin-ads outcomes = %v, want [%s]", got, dispatchOutcomeFailure)
	}
}

// TestJobTransitionMetricsRecordRunningAndTerminal pins that both the running
// transition and the terminal status reach the metrics, so a stuck-job alert can
// be built on the difference between them.
func TestJobTransitionMetricsRecordRunningAndTerminal(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	rec := &recordingMetrics{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: failDispatcher{},
	})
	orch.SetMetrics(rec)

	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)

	if !rec.sawJobState(model.JobRunning) {
		t.Error("the running transition was not recorded")
	}
	// One succeeded, one failed => partial.
	if !rec.sawJobState(model.JobPartial) {
		t.Error("the terminal partial status was not recorded")
	}
}

// TestDispatchPanicIsRecordedAsItsOwnOutcome pins that a recovered dispatcher
// panic is NOT folded into the ordinary failure bucket. A panic is a bug in this
// service; an upstream refusal is not, and collapsing them hides the former in
// the noise of the latter.
func TestDispatchPanicIsRecordedAsItsOwnOutcome(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	rec := &recordingMetrics{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: panicDispatcher{},
	})
	orch.SetMetrics(rec)

	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds}, nil)
	waitForTerminal(t, jobs, id)

	got := rec.outcomesFor(model.ProviderGoogleAds)
	if len(got) != 1 || got[0] != dispatchOutcomePanic {
		t.Errorf("google-ads outcomes = %v, want [%s]", got, dispatchOutcomePanic)
	}
}

// TestSkippedDispatchIsNotCountedAsSuccess pins the ordering inside
// dispatchOutcomeFor: a skipped platform is reported with OK=true, so checking OK
// first would inflate the dispatch success rate with work never attempted.
func TestSkippedDispatchIsNotCountedAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  platformResult
		want string
	}{
		{"skipped wins over ok", platformResult{OK: true, Skipped: true}, dispatchOutcomeSkipped},
		{"plain success", platformResult{OK: true}, dispatchOutcomeSuccess},
		{"failure", platformResult{OK: false}, dispatchOutcomeFailure},
		{"skipped and not ok", platformResult{OK: false, Skipped: true}, dispatchOutcomeSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatchOutcomeFor(tc.res); got != tc.want {
				t.Errorf("dispatchOutcomeFor(%+v) = %q, want %q", tc.res, got, tc.want)
			}
		})
	}
}

// TestSetMetricsNilRestoresNoop pins that clearing the recorder does not leave a
// nil that the record sites would have to guard against.
func TestSetMetricsNilRestoresNoop(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), nil)
	orch.SetMetrics(&recordingMetrics{})
	orch.SetMetrics(nil)
	if orch.dispatchMetrics() == nil {
		t.Fatal("dispatchMetrics() returned nil after SetMetrics(nil)")
	}
	// Must not panic.
	orch.dispatchMetrics().RecordDispatch(context.Background(), model.ProviderGoogleAds, dispatchOutcomeSuccess)
}

// TestOrchestratorDefaultsToNoopMetrics pins that an orchestrator built without
// SetMetrics records safely, which is what every existing test relies on.
func TestOrchestratorDefaultsToNoopMetrics(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), nil)
	if orch.dispatchMetrics() == nil {
		t.Fatal("a freshly constructed orchestrator has a nil metrics recorder")
	}
	orch.dispatchMetrics().RecordJobTransition(context.Background(), model.JobSucceeded)
}

// upstreamCapableDispatcher implements every OPTIONAL capability the orchestrator
// instruments (StatusToggler, MetricsReader, SettingsReader, AccountLister,
// EmailSearcher and CampaignAdopter) so one fake can drive every instrumented call
// site. err is returned by whichever capability is exercised, letting a table flip a
// case from the success arm to the error arm without a second type.
//
// It must implement EVERY instrumented capability: a capability it does not satisfy
// fails the orchestrator's type assertion and returns "unsupported" BEFORE any
// upstream call is recorded, which would turn that path's coverage case into a test
// of the pre-platform guard instead.
type upstreamCapableDispatcher struct{ err error }

func (d upstreamCapableDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d upstreamCapableDispatcher) ToggleStatus(context.Context, string, model.Provider, *model.Campaign, string) error {
	return d.err
}

func (d upstreamCapableDispatcher) ReadMetrics(context.Context, string, model.Provider, *model.Campaign, model.MetricsWindow) (*model.CampaignMetrics, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.CampaignMetrics{}, nil
}

func (d upstreamCapableDispatcher) ReadSettings(context.Context, string, model.Provider, *model.Campaign) (*model.CampaignSettingsReadback, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.CampaignSettingsReadback{}, nil
}

func (d upstreamCapableDispatcher) ListAccounts(context.Context, string, model.Provider) ([]model.AccessibleAccount, error) {
	if d.err != nil {
		return nil, d.err
	}
	return []model.AccessibleAccount{}, nil
}

func (d upstreamCapableDispatcher) SearchEmails(context.Context, string, model.Provider, string) ([]model.MarketingEmail, error) {
	if d.err != nil {
		return nil, d.err
	}
	return []model.MarketingEmail{}, nil
}

func (d upstreamCapableDispatcher) SearchCampaigns(context.Context, string, model.Provider, string) (model.HubSpotCampaignPage, error) {
	if d.err != nil {
		return model.HubSpotCampaignPage{}, d.err
	}
	// Non-nil Campaigns: the orchestrator refuses a nil slice as a contract violation, and the
	// success arm asserts the call actually succeeded.
	return model.HubSpotCampaignPage{Campaigns: []model.HubSpotCampaign{}}, nil
}

func (d upstreamCapableDispatcher) CreateCampaign(context.Context, string, model.Provider, string) (*model.HubSpotCampaign, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.HubSpotCampaign{ID: "1", Name: "n"}, nil
}

func (d upstreamCapableDispatcher) ReadKeywordPerformance(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.KeywordPerformance, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.KeywordPerformance{}, nil
}

func (d upstreamCapableDispatcher) ReadAudienceInsights(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (*model.AudienceInsights, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.AudienceInsights{}, nil
}

// ApplyKeywordActions returns ONE outcome per requested action, which is the contract the
// orchestrator enforces. A fake returning an empty slice makes every "success" call fail with
// unconfirmedOutcomeCountError, so a test that does not assert the success arm's error would
// pass while the operation it claims to instrument actually failed.
func (d upstreamCapableDispatcher) ApplyKeywordActions(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, actions []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	if d.err != nil {
		return nil, d.err
	}
	out := make([]model.KeywordActionOutcome, 0, len(actions))
	for _, a := range actions {
		out = append(out, model.KeywordActionOutcome{
			AdGroupID:   a.AdGroupID,
			CriterionID: a.CriterionID,
			Action:      a.Action,
		})
	}
	return out, nil
}

func (d upstreamCapableDispatcher) LookupCampaign(context.Context, string, model.Provider, string) (*model.PlatformCampaignRef, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &model.PlatformCampaignRef{ID: "pc-1", Name: "n"}, nil
}

// TestUpstreamCallsAreInstrumented drives each instrumented capability path and
// asserts the upstream call was actually recorded with the right bounded operation
// token and outcome.
//
// Without this the instrumentation is vacuous scaffolding: recordUpstream could stop
// being called, pass the wrong constant, or invert the error->outcome mapping, and
// every other test in the suite would stay green — the failure mode a metric has when
// nothing asserts it is that it silently stops recording.
//
// The table's COMPLETENESS is derived, not asserted against a hand-written number:
// recordUpstreamOperations parses the orchestrator source for the operation tokens
// actually passed to recordUpstream, so a newly instrumented path fails this test in
// the very commit that adds it. A literal count could not do that — both the count and
// the table are hand-maintained, so adding a path moves neither.
func TestUpstreamCallsAreInstrumented(t *testing.T) {
	const platform = model.ProviderRedditAds
	campaign := &model.Campaign{PlatformCampaignID: "pc-1", CampaignName: "n"}

	// Each case drives ONE capability path through the orchestrator and names the
	// operation token that path must record.
	cases := []struct {
		name string
		op   string
		call func(context.Context, *Orchestrator) error
	}{
		{
			name: "toggle status",
			op:   opToggleStatus,
			call: func(ctx context.Context, o *Orchestrator) error {
				return o.ToggleCampaignStatus(ctx, "p1", platform, campaign, model.CampaignRunPaused)
			},
		},
		{
			name: "read metrics",
			op:   opReadMetrics,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.ReadCampaignMetrics(ctx, "p1", platform, campaign, model.MetricsWindowLast7Days)
				return err
			},
		},
		{
			name: "read settings",
			op:   opReadSettings,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.ReadCampaignSettings(ctx, "p1", platform, campaign)
				return err
			},
		},
		{
			name: "list accounts",
			op:   opListAccounts,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.ReadAccounts(ctx, "p1", platform)
				return err
			},
		},
		{
			name: "search emails",
			op:   opSearchEmails,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.SearchEmails(ctx, "p1", platform, "q")
				return err
			},
		},
		{
			name: "read keywords",
			op:   opReadKeywords,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.ReadKeywordPerformance(ctx, "p1", platform, model.MetricsWindowLast7Days)
				return err
			},
		},
		{
			name: "read audience",
			op:   opReadAudience,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.ReadAudienceInsights(ctx, "p1", platform, model.MetricsWindowLast7Days)
				return err
			},
		},
		{
			name: "keyword actions",
			op:   opKeywordActions,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.ApplyKeywordActions(ctx, "p1", platform, campaign, []model.KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: model.KeywordActionPause}})
				return err
			},
		},
		{
			name: "search campaign",
			op:   opSearchCampaign,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.SearchCampaigns(ctx, "p1", platform, "q")
				return err
			},
		},
		{
			// The create is the most operationally sensitive call the service makes: it spends
			// into a portal-wide namespace, and an unconfirmed one may have committed. Its
			// latency and failure count are exactly what an operator needs.
			name: "create campaign",
			op:   opCreateCampaign,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.CreateCampaign(ctx, "p1", platform, "n")
				return err
			},
		},
		{
			name: "lookup campaign",
			op:   opLookupCampaign,
			call: func(ctx context.Context, o *Orchestrator) error {
				_, err := o.LookupPlatformCampaign(ctx, "p1", platform, "pc-1")
				return err
			},
		},
	}

	// Completeness gate: every operation token recordUpstream is called with in the
	// orchestrator source must have a case above. Derived from source so it tracks the
	// instrumentation itself rather than an edit to this file.
	covered := make(map[string]bool, len(cases))
	for _, tc := range cases {
		covered[tc.op] = true
	}
	for _, op := range recordUpstreamOperations(t) {
		if !covered[op] {
			t.Errorf("recordUpstream is called with operation %q in the orchestrator, but no case "+
				"below drives it: the instrumentation on that path is unguarded, so removing the "+
				"call, using the wrong token, or inverting its outcome would pass", op)
		}
	}

	for _, tc := range cases {
		for _, arm := range []struct {
			name        string
			platformErr error
			wantOutcome string
		}{
			{name: "success", platformErr: nil, wantOutcome: callOutcomeOK},
			{name: "error", platformErr: errors.New("upstream boom"), wantOutcome: callOutcomeError},
		} {
			t.Run(tc.name+"/"+arm.name, func(t *testing.T) {
				rec := &recordingMetrics{}
				// A non-empty campaign scope: the two insight reads answer an empty scope
				// WITHOUT an upstream call, so with the default fake they would record
				// nothing and this instrumentation assertion would fail for the right
				// reason but the wrong cause.
				orch := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"555"}}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
					platform: upstreamCapableDispatcher{err: arm.platformErr},
				})
				orch.SetMetrics(rec)

				err := tc.call(context.Background(), orch)
				if arm.platformErr != nil && err == nil {
					t.Fatalf("%s: expected the platform error to surface", tc.name)
				}
				// The success arm must actually SUCCEED. Without this, a fake that violates a
				// downstream contract (e.g. returning fewer outcomes than actions) makes the
				// call fail while the subtest still passes — the instrumentation would be
				// asserted over an operation that errored, and a post-dispatch regression
				// would stay green.
				if arm.platformErr == nil && err != nil {
					t.Fatalf("%s: success arm returned an error, so this asserts instrumentation over a failed call: %v", tc.name, err)
				}

				got := rec.upstreamCalls()
				if len(got) != 1 {
					t.Fatalf("recorded %d upstream calls, want exactly 1: %+v", len(got), got)
				}
				if got[0].operation != tc.op {
					t.Errorf("operation = %q, want %q", got[0].operation, tc.op)
				}
				if got[0].outcome != arm.wantOutcome {
					t.Errorf("outcome = %q, want %q", got[0].outcome, arm.wantOutcome)
				}
				if got[0].platform != platform {
					t.Errorf("platform = %q, want %q", got[0].platform, platform)
				}
				if got[0].seconds < 0 {
					t.Errorf("seconds = %v, want a non-negative duration", got[0].seconds)
				}
			})
		}
	}
}

// TestPrePlatformGuardsAreNotInstrumented pins that a refusal which never reaches the
// ad platform records NO upstream call.
//
// This is the other half of the histogram's meaning: a "no dispatcher registered" or
// "capability unsupported" rejection returns in nanoseconds, so counting it would both
// inflate the error rate with local refusals and drag every latency quantile toward
// zero. recordUpstream is deliberately called only AFTER the guards pass.
func TestPrePlatformGuardsAreNotInstrumented(t *testing.T) {
	campaign := &model.Campaign{PlatformCampaignID: "pc-1", CampaignName: "n"}

	cases := []struct {
		name        string
		dispatchers map[model.Provider]PlatformDispatcher
		call        func(context.Context, *Orchestrator) error
	}{
		{
			name:        "no dispatcher registered for platform",
			dispatchers: map[model.Provider]PlatformDispatcher{},
			call: func(ctx context.Context, o *Orchestrator) error {
				return o.ToggleCampaignStatus(ctx, "p1", model.ProviderRedditAds, campaign, model.CampaignRunPaused)
			},
		},
		{
			// okDispatcher implements PlatformDispatcher but none of the optional
			// capabilities, so the type assertion fails before any platform call.
			name:        "dispatcher does not implement the capability",
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: okDispatcher{}},
			call: func(ctx context.Context, o *Orchestrator) error {
				return o.ToggleCampaignStatus(ctx, "p1", model.ProviderRedditAds, campaign, model.CampaignRunPaused)
			},
		},
		{
			name:        "campaign not provisioned upstream",
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: upstreamCapableDispatcher{}},
			call: func(ctx context.Context, o *Orchestrator) error {
				return o.ToggleCampaignStatus(ctx, "p1", model.ProviderRedditAds, &model.Campaign{}, model.CampaignRunPaused)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), tc.dispatchers)
			orch.SetMetrics(rec)

			if err := tc.call(context.Background(), orch); err == nil {
				t.Fatal("expected the pre-platform guard to return an error")
			}
			if got := rec.upstreamCalls(); len(got) != 0 {
				t.Fatalf("a pre-platform guard recorded %d upstream calls, want 0: %+v", len(got), got)
			}
		})
	}
}

// terminalWriteFailsJobRepo lets the RUNNING status write succeed but fails every
// TERMINAL one, reproducing the database blip that leaves a row stuck at `running`
// while dispatch itself completed.
type terminalWriteFailsJobRepo struct {
	*fakeJobRepo
}

func (r *terminalWriteFailsJobRepo) UpdateJobStatus(ctx context.Context, id string, status model.JobStatus, result []byte, jobErr string) error {
	if status.Terminal() {
		return errors.New("terminal status write failed")
	}
	return r.fakeJobRepo.UpdateJobStatus(ctx, id, status, result, jobErr)
}

// TestTerminalTransitionNotRecordedWhenWriteFails pins the guard that keeps the
// stuck-job alert honest.
//
// campaign_job_transitions_total exists so a stuck job shows up as the GAP between
// the running count and the terminal count. When the finalizing write fails the row
// stays `running` in the database and is precisely the stuck job the alert hunts, so
// recording its terminal here would close the gap for exactly the rows the metric
// exists to expose -- the alert would go quiet at the moment it should fire.
func TestTerminalTransitionNotRecordedWhenWriteFails(t *testing.T) {
	jobs := &terminalWriteFailsJobRepo{fakeJobRepo: newFakeJobRepo()}
	rec := &recordingMetrics{}
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	orch.SetMetrics(rec)

	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The job can never reach a terminal status (every terminal write fails), so wait
	// on the RUNNING transition instead and then let the finalize attempt land.
	deadline := time.Now().Add(2 * time.Second)
	for !rec.sawJobState(model.JobRunning) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !rec.sawJobState(model.JobRunning) {
		t.Fatal("the running transition was never recorded")
	}
	// Give the finalize path time to run and (wrongly) record, if the guard is absent.
	time.Sleep(200 * time.Millisecond)

	j, _ := jobs.GetJob(context.Background(), "", id)
	if j.Status.Terminal() {
		t.Fatalf("precondition failed: job reached terminal status %q despite the failing write", j.Status)
	}

	for _, s := range []model.JobStatus{model.JobSucceeded, model.JobFailed, model.JobPartial} {
		if rec.sawJobState(s) {
			t.Errorf("terminal transition %q was recorded even though the status write failed", s)
		}
	}
}

// panicOnRecordDispatchMetrics panics from RecordDispatch — the one call that runs
// AFTER dispatchPlatform has returned and its result has been stored.
type panicOnRecordDispatchMetrics struct{ recordingMetrics }

func (m *panicOnRecordDispatchMetrics) RecordDispatch(context.Context, model.Provider, string) {
	panic("metrics recorder exploded")
}

// TestPostDispatchRecordingPanicDoesNotRewriteSuccess pins that a panic raised after
// a successful dispatch cannot degrade that platform's result to a failure.
//
// The campaign really was created upstream. Reporting it as failed would invite a
// reconcile or retry that could DOUBLE-CREATE a paid campaign, so losing the metric
// is the strictly cheaper failure. This also keeps the recovery honest: the panic is
// still recovered, so it can never crash the process or cancel sibling platforms.
func TestPostDispatchRecordingPanicDoesNotRewriteSuccess(t *testing.T) {
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	orch.SetMetrics(&panicOnRecordDispatchMetrics{})

	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)

	var got []platformResult
	if uerr := json.Unmarshal(j.Result, &got); uerr != nil {
		t.Fatalf("unmarshal job result: %v", uerr)
	}
	if len(got) != 1 {
		t.Fatalf("got %d platform results, want 1: %+v", len(got), got)
	}
	if !got[0].OK {
		t.Errorf("a panic in the post-dispatch recording call rewrote a SUCCESS into %q", got[0].Error)
	}
	if j.Status != model.JobSucceeded {
		t.Errorf("job status = %q, want %q", j.Status, model.JobSucceeded)
	}
}

// TestTerminalizeRecordsOnlyOnSuccessfulWrite covers BOTH finalize paths -- the
// normal one and the marshal-failure one -- at the single point they share.
//
// The marshal-failure arm is unreachable through a normal dispatch (platformResult
// is all plain types, so json.Marshal cannot fail on it), which is exactly why it
// is exercised here directly: without this, that arm's guard could be reverted to
// unconditional recording and the whole suite would stay green.
func TestTerminalizeRecordsOnlyOnSuccessfulWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		writeFails bool
		status     model.JobStatus
		wantRecord bool
	}{
		{"successful write records", false, model.JobSucceeded, true},
		{"failed write records nothing", true, model.JobSucceeded, false},
		{"marshal-failure path, write succeeds", false, model.JobFailed, true},
		{"marshal-failure path, write fails", true, model.JobFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			var jobs domain.JobRepository = newFakeJobRepo()
			if tc.writeFails {
				jobs = &terminalWriteFailsJobRepo{fakeJobRepo: newFakeJobRepo()}
			}
			orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, nil)
			orch.SetMetrics(rec)
			if _, err := jobs.CreateJob(context.Background(), "b1"); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}

			orch.terminalize(context.Background(), "job-b", tc.status, nil, "")

			if got := rec.sawJobState(tc.status); got != tc.wantRecord {
				t.Errorf("recorded transition %q = %v, want %v", tc.status, got, tc.wantRecord)
			}
		})
	}
}

// sweepingJobRepo reports a fixed number of rows recovered by FailStuckJobs, so a
// test can drive the sweeper's recording path without a database.
type sweepingJobRepo struct {
	*fakeJobRepo
	recovered int64
}

func (r *sweepingJobRepo) FailStuckJobs(context.Context, string) (int64, error) {
	return r.recovered, nil
}

// TestRecoverySweepClosesTheTransitionGap pins that the sweeper records a terminal
// transition for every row it recovers.
//
// The finalize path deliberately does NOT record a terminal whose status write
// failed, which leaves the running->terminal gap open for exactly the stuck rows.
// This is where that gap CLOSES. Without it the gap is permanent and a stuck-job
// alert keeps firing after the rows are already terminal in the database -- the
// mirror image of the bug the finalize guard fixes, and just as misleading.
func TestRecoverySweepClosesTheTransitionGap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovered int64
		want      int
	}{
		{"nothing stuck records nothing", 0, 0},
		{"one recovered row", 1, 1},
		{"every recovered row is counted", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &sweepingJobRepo{fakeJobRepo: newFakeJobRepo(), recovered: tc.recovered}
			rec := &recordingMetrics{}
			orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, nil)
			orch.SetMetrics(rec)

			// Drive ONE pass directly: the ticker interval is minutes, so going through
			// StartRecoverySweeper would either sleep for minutes or assert nothing.
			orch.runRecoverySweep()

			if got := countJobStates(rec, model.JobFailed); got != tc.want {
				t.Errorf("sweep recorded %d terminal transitions for %d recovered rows, want %d",
					got, tc.recovered, tc.want)
			}
		})
	}
}

// countJobStates counts recorded transitions matching s.
func countJobStates(m *recordingMetrics, s model.JobStatus) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, got := range m.jobStates {
		if got == s {
			n++
		}
	}
	return n
}

// recordUpstreamOperations returns the operation tokens actually passed to
// o.recordUpstream in this package's non-test sources, resolving each argument
// identifier (opReadSettings, ...) to its declared string constant.
//
// Derived from source rather than hand-listed for the reason the cs#164 provenance
// guard was rewritten: a hand-maintained roster and a hand-maintained table move
// together only by luck, so the assertion detects edits to the table instead of
// changes to the thing the table tracks.
//
// It fails on an empty result rather than returning one: a broken scan would make the
// completeness gate silently vacuous, which is the exact failure mode being fixed.
func recordUpstreamOperations(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	// Constant name -> declared value, so an identifier argument can be resolved to
	// the token that actually reaches the metrics recorder.
	consts := map[string]string{}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						continue
					}
					consts[ident.Name] = v
				}
			}
		}
	}

	seen := map[string]bool{}
	var ops []string
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "recordUpstream" {
				return true
			}
			// recordUpstream(ctx, platform, operation, start, err) — operation is 3rd.
			if len(call.Args) < 3 {
				t.Fatalf("recordUpstream call with %d args: the operation argument position "+
					"this scan relies on has changed", len(call.Args))
			}
			ident, ok := call.Args[2].(*ast.Ident)
			if !ok {
				t.Fatalf("recordUpstream operation argument is not a plain identifier (%T); "+
					"this scan can no longer resolve the token", call.Args[2])
			}
			v, ok := consts[ident.Name]
			if !ok {
				t.Fatalf("recordUpstream is passed %s, which is not a string constant declared "+
					"in this package", ident.Name)
			}
			if !seen[v] {
				seen[v] = true
				ops = append(ops, v)
			}
			return true
		})
	}

	if len(ops) == 0 {
		t.Fatal("found no recordUpstream call sites in this package; the source scan is broken, " +
			"and an empty roster would make the completeness gate vacuous")
	}
	sort.Strings(ops)
	return ops
}
