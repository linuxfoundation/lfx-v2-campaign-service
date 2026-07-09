// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeJobRepo records job status transitions.
type fakeJobRepo struct {
	mu      sync.Mutex
	jobs    map[string]*model.CampaignJob
	counter int
}

func newFakeJobRepo() *fakeJobRepo { return &fakeJobRepo{jobs: map[string]*model.CampaignJob{}} }

func (r *fakeJobRepo) CreateJob(_ context.Context, briefID string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	id := "job-" + string(rune('a'+r.counter))
	j := &model.CampaignJob{ID: id, BriefID: briefID, Status: model.JobQueued}
	r.jobs[id] = j
	return j, nil
}

func (r *fakeJobRepo) GetJob(_ context.Context, id string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, errors.New("not found")
	}
	// Return a snapshot so callers don't race with concurrent UpdateJobStatus.
	cp := *j
	return &cp, nil
}

func (r *fakeJobRepo) UpdateJobStatus(_ context.Context, id string, status model.JobStatus, result []byte, jobErr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	j.Status = status
	j.Result = result
	j.Error = jobErr
	return nil
}

// fakeCampaignRepo records upserted campaigns.
type fakeCampaignRepo struct {
	mu       sync.Mutex
	upserted []*model.Campaign
}

func (r *fakeCampaignRepo) GetCampaign(context.Context, string, string, string) (*model.Campaign, error) {
	return nil, errors.New("unused")
}
func (r *fakeCampaignRepo) UpsertCampaign(_ context.Context, c *model.Campaign) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Version = 1
	r.upserted = append(r.upserted, c)
	return c, nil
}
func (r *fakeCampaignRepo) ReplaceCampaign(context.Context, *model.Campaign, int64) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

// okDispatcher always succeeds.
type okDispatcher struct{}

func (okDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// failDispatcher always fails.
type failDispatcher struct{}

func (failDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("boom")
}

func waitForTerminal(t *testing.T, jobs *fakeJobRepo, id string) *model.CampaignJob {
	t.Helper()
	for i := 0; i < 100; i++ {
		j, _ := jobs.GetJob(context.Background(), id)
		if j.Status.Terminal() {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not reach terminal status")
	return nil
}

func TestOrchestrator_AllSucceed(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(nil, camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
	if len(camps.upserted) != 2 {
		t.Errorf("upserted %d campaigns, want 2", len(camps.upserted))
	}
}

func TestOrchestrator_PartialFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(nil, camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: failDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobPartial {
		t.Errorf("status = %s, want partial", j.Status)
	}
}

func TestOrchestrator_NoDispatcherFails(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(nil, camps, jobs, nil) // no dispatchers
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
}

func TestAggregateStatus(t *testing.T) {
	cases := []struct {
		name    string
		results []platformResult
		want    model.JobStatus
	}{
		{"all ok", []platformResult{{OK: true}, {OK: true}}, model.JobSucceeded},
		{"all fail", []platformResult{{OK: false}, {OK: false}}, model.JobFailed},
		{"mixed", []platformResult{{OK: true}, {OK: false}}, model.JobPartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateStatus(tc.results); got != tc.want {
				t.Errorf("aggregateStatus = %s, want %s", got, tc.want)
			}
		})
	}
}
