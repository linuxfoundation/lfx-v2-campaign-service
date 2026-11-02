// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/membudget"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestNewUploadBudget pins how the container sizes the per-pod upload memory budget (#179):
// UPLOAD_MEMORY_BUDGET_BYTES wins, else 40% of the cgroup limit, else 40% of the chart's
// default; a malformed value stops startup rather than falling back silently.
func TestNewUploadBudget(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == membudget.EnvBudgetBytes {
				return v
			}
			return ""
		}
	}
	noCgroup := func() (int64, bool) { return 0, false }

	b, err := newUploadBudget(env(""), func() (int64, bool) { return 1 << 30, true })
	if err != nil || b.Total() != (int64(1)<<30)/100*40 {
		t.Fatalf("cgroup-derived budget = %d (err %v), want 40%% of 1 GiB", b.Total(), err)
	}
	if b, err = newUploadBudget(env(""), noCgroup); err != nil || b.Total() != constants.UploadMemoryBudgetFallbackBytes {
		t.Fatalf("fallback budget = %d (err %v), want %d", b.Total(), err, constants.UploadMemoryBudgetFallbackBytes)
	}
	if b, err = newUploadBudget(env("300Mi"), noCgroup); err != nil || b.Total() != 300<<20 {
		t.Fatalf("env budget = %d (err %v), want 300 MiB", b.Total(), err)
	}
	if _, err = newUploadBudget(env("three hundred"), noCgroup); err == nil {
		t.Fatal("a malformed UPLOAD_MEMORY_BUDGET_BYTES must fail startup")
	}
}

// TestContainer_UploadBudgetIsSharedAndExported pins the container's half of #179's wiring, by
// identity rather than presence: the budget NewContainer builds is the one the live-wiring helper
// binds into the brief service's decode reserver, and the one the metrics registry exports.
// DecodeReserverIsSet would be true for a private budget too, which is the defect #179 removed.
func TestContainer_UploadBudgetIsSharedAndExported(t *testing.T) {
	cont, err := NewContainer(&config.Config{Host: "*", Port: "8080"})
	require.NoError(t, err)
	require.NotNil(t, cont.UploadBudget, "NewContainer must build the per-pod upload budget")

	s := service.NewBriefService(nil, nil, nil, nil)
	bindBriefLiveBackends(s, cont.UploadBudget, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	assert.True(t, s.SharesUploadBudget(cont.UploadBudget),
		"the decode reserver must charge the container's UploadBudget, not a budget of its own")

	rec := httptest.NewRecorder()
	cont.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, rec.Body.String(), "campaign_upload_memory_budget_bytes",
		"the container must wire its UploadBudget into the metrics registry")
}

// TestNewUploadBudget_KeepsAPixelReserveAndRaisesSmallPodsToTheFloor: the production budget keeps
// one worst-case pixel charge out of reach of the wire stage, and a pod whose 40% share is below
// one worst-case upload is raised to it (and logged) rather than refusing every large upload.
func TestNewUploadBudget_KeepsAPixelReserveAndRaisesSmallPodsToTheFloor(t *testing.T) {
	noEnv := func(string) string { return "" }
	b, err := newUploadBudget(noEnv, func() (int64, bool) { return 256 << 20, true })
	require.NoError(t, err)
	assert.Equal(t, constants.UploadWorstCaseBytes, b.Total(), "40%% of 256 MiB is below one worst-case upload and must be raised")
	assert.Equal(t, constants.UploadWorstCaseBytes-constants.CreativeDecodePeakBytes, b.FirstStageLimit(),
		"the wire stage must leave one worst-case pixel charge free")
	assert.GreaterOrEqual(t, b.FirstStageLimit(), constants.UploadAdmissionWeightBytes,
		"the wire stage must still admit one worst-case wire charge")
}
