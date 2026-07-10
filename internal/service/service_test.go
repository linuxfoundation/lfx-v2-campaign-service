// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"
	"time"

	campaignsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceReady(t *testing.T) {
	tests := []struct {
		name    string
		service *CampaignService
		want    bool
	}{
		{
			name:    "ready when constructed",
			service: NewCampaignService(nil),
			want:    true,
		},
		{
			name:    "not ready when flag false",
			service: &CampaignService{ready: false},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.service.ServiceReady())
		})
	}
}

func TestLivez(t *testing.T) {
	s := NewCampaignService(nil)

	result, err := s.Livez(context.Background())

	assert.NoError(t, err)
	assert.Equal(t, "OK\n", string(result))
}

func TestLivez_IgnoresUnhealthyDependency(t *testing.T) {
	dep := &countingReadiness{ready: false}
	s := NewCampaignService(dep)

	result, err := s.Livez(context.Background())

	assert.NoError(t, err)
	assert.Equal(t, "OK\n", string(result))
	assert.Equal(t, 0, dep.calls, "Livez must not call the database dependency")
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name         string
		service      *CampaignService
		expectError  bool
		expectedBody string
	}{
		{
			name:         "ready returns OK",
			service:      NewCampaignService(nil),
			expectError:  false,
			expectedBody: "OK\n",
		},
		{
			name:         "ready with healthy dependency returns OK",
			service:      NewCampaignService(fakeReadiness{ready: true}),
			expectError:  false,
			expectedBody: "OK\n",
		},
		{
			name:        "not ready returns ServiceUnavailable",
			service:     &CampaignService{ready: false},
			expectError: true,
		},
		{
			name:        "unhealthy dependency returns ServiceUnavailable",
			service:     NewCampaignService(fakeReadiness{ready: false}),
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.service.Readyz(context.Background())

			if tt.expectError {
				assert.Error(t, err)
				var unavailable *campaignsvc.ServiceUnavailableError
				assert.ErrorAs(t, err, &unavailable)
				assert.Nil(t, result)
				assert.NotContains(t, err.Error(), "PGPASSWORD")
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedBody, string(result))
		})
	}
}

func TestReadyz_DependencyTimeout(t *testing.T) {
	// A dependency that blocks until the request context is canceled proves
	// Readyz applies readinessProbeTimeout (removing the deadline would hang).
	dep := &blockingReadiness{}
	s := NewCampaignService(dep)

	start := time.Now()
	result, err := s.Readyz(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Nil(t, result)
	var unavailable *campaignsvc.ServiceUnavailableError
	assert.ErrorAs(t, err, &unavailable)
	assert.True(t, dep.sawCanceled, "dependency must observe context cancellation")
	assert.GreaterOrEqual(t, elapsed, readinessProbeTimeout)
}

// fakeReadiness is a ReadinessChecker whose result is controllable.
type fakeReadiness struct{ ready bool }

func (f fakeReadiness) Ready(context.Context) bool { return f.ready }

// countingReadiness records how many times Ready is called.
type countingReadiness struct {
	ready bool
	calls int
}

func (c *countingReadiness) Ready(context.Context) bool {
	c.calls++
	return c.ready
}

// blockingReadiness waits until ctx is done, then reports not ready.
type blockingReadiness struct {
	sawCanceled bool
}

func (b *blockingReadiness) Ready(ctx context.Context) bool {
	<-ctx.Done()
	b.sawCanceled = ctx.Err() != nil
	return false
}

func TestServiceReady_WithDependency(t *testing.T) {
	tests := []struct {
		name string
		dep  ReadinessChecker
		want bool
	}{
		{"healthy dependency", fakeReadiness{ready: true}, true},
		{"unhealthy dependency", fakeReadiness{ready: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewCampaignService(tt.dep)
			assert.Equal(t, tt.want, s.ServiceReady())
		})
	}
}
