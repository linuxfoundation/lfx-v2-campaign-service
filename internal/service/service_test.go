// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	campaignsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/stretchr/testify/assert"
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
	s := NewCampaignService(fakeReadiness{ready: false})

	result, err := s.Livez(context.Background())

	assert.NoError(t, err)
	assert.Equal(t, "OK\n", string(result))
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

// fakeReadiness is a ReadinessChecker whose result is controllable.
type fakeReadiness struct{ ready bool }

func (f fakeReadiness) Ready(context.Context) bool { return f.ready }

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
