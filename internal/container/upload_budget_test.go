// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/membudget"
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
