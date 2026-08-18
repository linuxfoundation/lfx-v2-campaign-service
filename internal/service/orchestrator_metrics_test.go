// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sync"
	"testing"

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
